package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

// Wait polls until the operation is done: an action until DigitalOcean reports it completed or
// errored, a delete until the Droplet answers 404, a create without an action until the Droplet
// is active. The pause starts at the poll interval and grows to five times it, which keeps a
// long resize cheap in requests. A rate-limited poll is not an answer, so Wait keeps polling
// through it; any other error ends the wait.
func (p *Provider) Wait(ctx context.Context, op cloud.Operation) error {
	if op.Done() {
		return nil
	}
	check, err := p.checker(op)
	if err != nil {
		return err
	}
	pause := p.poll
	for {
		done, err := check(ctx)
		switch {
		case err != nil && errors.Is(err, cloud.ErrRateLimited):
		case err != nil:
			if ctx.Err() != nil {
				return fmt.Errorf("digitalocean: waiting for %s of %s: %w", op.Kind, op.Resource, ctx.Err())
			}
			return err
		case done:
			return nil
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("digitalocean: waiting for %s of %s: %w", op.Kind, op.Resource, ctx.Err())
		case <-timer.C:
		}
		if pause = pause * 3 / 2; pause > 5*p.poll {
			pause = 5 * p.poll
		}
	}
}

// checker returns the poll for an operation: done, or an error that ends the wait.
func (p *Provider) checker(op cloud.Operation) (func(context.Context) (bool, error), error) {
	switch {
	case strings.HasPrefix(op.ID, opDropletGone):
		id := strings.TrimPrefix(op.ID, opDropletGone)
		return func(ctx context.Context) (bool, error) {
			_, err := p.droplet(ctx, id)
			if errors.Is(err, cloud.ErrNotFound) {
				return true, nil
			}
			return false, err
		}, nil
	case strings.HasPrefix(op.ID, opDropletActive):
		id := strings.TrimPrefix(op.ID, opDropletActive)
		return func(ctx context.Context) (bool, error) {
			d, err := p.droplet(ctx, id)
			if err != nil {
				return false, err
			}
			return d.Status == "active", nil
		}, nil
	}
	n, err := strconv.Atoi(op.ID)
	if err != nil || n < 1 {
		return nil, fmt.Errorf("digitalocean: %q is not an operation this provider made: %w", op.ID, cloud.ErrInvalid)
	}
	return func(ctx context.Context) (bool, error) {
		a, _, err := p.c.Actions.Get(ctx, n)
		if err != nil {
			return false, apiErr("get action "+op.ID, err)
		}
		switch a.Status {
		case godo.ActionCompleted:
			return true, nil
		case "errored":
			return false, fmt.Errorf("digitalocean: %s action %d on %s: %w", a.Type, a.ID, op.Resource, cloud.ErrOperationFailed)
		}
		return false, nil
	}, nil
}

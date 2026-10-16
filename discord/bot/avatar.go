package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/omit"
)

// DefaultCDNURL is where Discord serves avatars and application icons.
const DefaultCDNURL = "https://cdn.discordapp.com"

// MaxAvatarBytes bounds discord.avatar_file: Discord refuses larger uploads, and the file is read
// whole.
const MaxAvatarBytes = 10 << 20

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// syncAvatar gives the bot user and the application the image at discord.avatar_file (gravel#158),
// writing only what differs, so a restart writes nothing. It is a job that never fails the bot: a
// missing file, a refused upload or Discord's avatar rate limit is logged and the bot runs on.
//
// Discord's image hash is not one gravel can compute, so the two are compared differently. The
// bot user's current avatar is fetched from the CDN at full size, which serves the uploaded bytes
// unchanged, and compared with the file. An application icon is stored downscaled, so it is
// compared by hash instead: Discord gives one image the same hash as an avatar and as an icon, so
// once the avatar holds the file, an icon with another hash is set.
func (rt *Runtime) syncAvatar(ctx context.Context) error {
	path := rt.cfg.Discord.AvatarFile
	log := rt.logger.With("avatar_file", path)
	img, err := readAvatar(path)
	if err != nil {
		log.Error("the bot's avatar is left as it is", "error", err.Error())
		return nil
	}
	r := rt.client.Rest
	me, err := r.GetCurrentUser("", rest.WithCtx(ctx))
	if err != nil {
		log.Error("cannot read the bot user; its avatar is left as it is", "error", err.Error())
		return nil
	}
	hash := ""
	if me.Avatar != nil {
		hash = *me.Avatar
	}
	same, err := rt.avatarIs(ctx, me.ID.String(), hash, img)
	if err != nil {
		log.Error("cannot compare the bot's avatar with the file; it is left as it is", "error", err.Error())
		return nil
	}
	if !same {
		u, err := r.UpdateCurrentUser(discord.UserUpdate{Avatar: omit.New(discord.NewIconRaw(discord.IconTypePNG, img))}, rest.WithCtx(ctx))
		if err != nil {
			log.Error("Discord refused the bot's avatar", "error", err.Error())
			return nil
		}
		if u.Avatar == nil {
			log.Error("Discord accepted the bot's avatar but reports none")
			return nil
		}
		hash = *u.Avatar
		log.Info("bot avatar set", "hash", hash)
	}
	app, err := r.GetCurrentApplication(rest.WithCtx(ctx))
	if err != nil {
		log.Error("cannot read the application; its icon is left as it is", "error", err.Error())
		return nil
	}
	if app.Icon != nil && *app.Icon == hash {
		if same {
			log.Debug("bot avatar and application icon already match the file")
		}
		return nil
	}
	a, err := r.UpdateCurrentApplication(discord.ApplicationUpdate{Icon: omit.New(discord.NewIconRaw(discord.IconTypePNG, img))}, rest.WithCtx(ctx))
	if err != nil {
		log.Error("Discord refused the application icon", "error", err.Error())
		return nil
	}
	icon := ""
	if a.Icon != nil {
		icon = *a.Icon
	}
	log.Info("application icon set", "hash", icon)
	return nil
}

// readAvatar reads the file, a PNG of at most MaxAvatarBytes.
func readAvatar(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	img, err := io.ReadAll(io.LimitReader(f, MaxAvatarBytes+1))
	if err != nil {
		return nil, err
	}
	switch {
	case len(img) > MaxAvatarBytes:
		return nil, fmt.Errorf("larger than %d MiB", MaxAvatarBytes>>20)
	case !bytes.HasPrefix(img, pngSignature):
		return nil, errors.New("not a PNG")
	}
	return img, nil
}

// avatarIs reports whether the user's avatar with this hash is the image: false for no avatar, an
// animated one, or one the CDN no longer has.
func (rt *Runtime) avatarIs(ctx context.Context, userID, hash string, img []byte) (bool, error) {
	if hash == "" || strings.HasPrefix(hash, "a_") {
		return false, nil
	}
	// The largest size Discord serves; an image smaller than that comes back as uploaded.
	url := fmt.Sprintf("%s/avatars/%s/%s.png?size=4096", rt.cdnURL, userID, hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := rt.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("the CDN answered %s", resp.Status)
	}
	cur, err := io.ReadAll(io.LimitReader(resp.Body, MaxAvatarBytes+1))
	if err != nil {
		return false, err
	}
	return bytes.Equal(cur, img), nil
}

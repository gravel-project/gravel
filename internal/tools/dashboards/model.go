package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The model gen writes and rules/check read back.

type fileDashboard struct {
	UID        string      `json:"uid"`
	Title      string      `json:"title"`
	Panels     []filePanel `json:"panels"`
	Templating struct {
		List []struct {
			Name       string `json:"name"`
			Definition string `json:"definition"`
		} `json:"list"`
	} `json:"templating"`
}

type filePanel struct {
	ID       int          `json:"id"`
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Optional string       `json:"gravelOptional"`
	Targets  []fileTarget `json:"targets"`
}

type fileTarget struct {
	RefID string `json:"refId"`
	Expr  string `json:"expr"`
}

func readDashboards(dir string) ([]fileDashboard, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no dashboards in %s", dir)
	}
	sort.Strings(paths)
	var out []fileDashboard
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var d fileDashboard
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, d)
	}
	return out, nil
}

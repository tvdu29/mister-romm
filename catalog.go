package main

import (
	"bufio"
	tea "charm.land/bubbletea/v2"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The catalog is a local index of what the server has (no ROM data): one file per platform,
// lines "id SEP name SEP size SEP sha1", sorted by name. Syncing only refreshes this index.

type catRow struct {
	id, name, sha1 string
	size           int64
}

type srvPlatform struct {
	ID       int    `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Display  string `json:"display_name"`
	RomCount int    `json:"rom_count"`
}

func (p srvPlatform) title() string {
	if p.Display != "" {
		return p.Display
	}
	return p.Name
}

func catalogPath(cfgDir, pid string) string {
	return filepath.Join(cfgDir, "catalog", pid+".tsv")
}

func readCatalog(path string) map[string]catRow {
	rows := map[string]catRow{}
	f, err := os.Open(path)
	if err != nil {
		return rows
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		id, rest, ok1 := strings.Cut(sc.Text(), "\x1f")
		name, rest, ok2 := strings.Cut(rest, "\x1f")
		sizeS, sha, _ := strings.Cut(rest, "\x1f")
		if !ok1 || !ok2 || id == "" || strings.Trim(id, "0123456789") != "" {
			continue // only well-formed lines
		}
		size, _ := strconv.ParseInt(sizeS, 10, 64)
		rows[id] = catRow{id: id, name: name, size: size, sha1: sha}
	}
	return rows
}

func writeCatalog(path string, rows map[string]catRow) error {
	list := make([]catRow, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := strings.ToLower(list[i].name), strings.ToLower(list[j].name)
		if a != b {
			return a < b
		}
		return list[i].id < list[j].id
	})
	var sb strings.Builder
	for _, r := range list {
		sb.WriteString(r.id + "\x1f" + r.name + "\x1f" + strconv.FormatInt(r.size, 10) + "\x1f" + r.sha1 + "\n")
	}
	return writeFileAtomic(path, []byte(sb.String()), 0o644)
}

// fetchPlatforms lists every platform of the server.
func fetchPlatforms(ctx context.Context, e env) ([]srvPlatform, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := e.get(ctx, e.client, "/api/platforms", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s", explainHTTP(resp))
	}
	var list []srvPlatform
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("the server answered with something that is not a platform list: %v", err)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].title() < list[j].title() })
	return list, nil
}

type syncProgress struct{ text string }
type syncDone struct {
	total, added int
	err          string
	cancelled    bool
	synced       map[string]string // platform id -> synced_at
}

// runSync refreshes the index of every selected platform. Incremental (server-side updated_after)
// unless full is set.
func runSync(ctx context.Context, e env, plats map[string]*selPlatform, full bool, send func(tea.Msg)) {
	done := syncDone{synced: map[string]string{}}
	for _, pid := range sortedKeys(plats) {
		p := plats[pid]
		path := catalogPath(e.cfgDir, pid)
		rows := map[string]catRow{}
		since := ""
		if !full {
			if old := readCatalog(path); len(old) > 0 && p.SyncedAt != "" {
				rows, since = old, p.SyncedAt
			}
		}
		before := len(rows)
		start := time.Now().UTC().Add(-60 * time.Second).Format("2006-01-02T15:04:05+00:00")
		offset, total := 0, 1
		for offset < total {
			send(syncProgress{text: fmt.Sprintf("Indexing %s: %d / %d (waiting for the server)", p.Name, offset, max(total, offset))})
			q := fmt.Sprintf("/api/roms?platform_ids=%s&limit=50&offset=%d&with_char_index=false&with_filter_values=false&with_rom_id_index=false", pid, offset)
			if since != "" {
				q += "&updated_after=" + url.QueryEscape(since)
			}
			pageCtx, cancelPage := context.WithTimeout(ctx, 180*time.Second) // a page carries full metadata: it can be slow
			resp, err := e.get(pageCtx, e.client, q, nil)
			if err != nil {
				cancelPage()
				done.err, done.cancelled = "indexing "+p.Name+": "+explainErr(err, clockOK()), ctx.Err() != nil
				send(done)
				return
			}
			if resp.StatusCode != 200 {
				done.err = "indexing " + p.Name + ": " + explainHTTP(resp)
				resp.Body.Close()
				cancelPage()
				send(done)
				return
			}
			var page struct {
				Items []struct {
					ID   int     `json:"id"`
					Name string  `json:"fs_name"`
					Size int64   `json:"fs_size_bytes"`
					SHA1 *string `json:"sha1_hash"`
				} `json:"items"`
				Total int `json:"total"`
			}
			err = json.NewDecoder(resp.Body).Decode(&page)
			resp.Body.Close()
			cancelPage()
			if err != nil {
				done.err = "indexing " + p.Name + ": unexpected answer (" + err.Error() + ")"
				send(done)
				return
			}
			for _, it := range page.Items {
				id := strconv.Itoa(it.ID)
				r := catRow{id: id, name: it.Name, size: it.Size}
				if it.SHA1 != nil {
					r.sha1 = *it.SHA1
				}
				rows[id] = r
			}
			total = page.Total
			if len(page.Items) == 0 {
				break
			}
			offset += len(page.Items)
		}
		if err := writeCatalog(path, rows); err != nil {
			done.err = "writing the catalog: " + err.Error()
			send(done)
			return
		}
		done.added += len(rows) - before
		done.synced[pid] = start
	}
	for _, pid := range sortedKeys(plats) {
		done.total += len(readCatalog(catalogPath(e.cfgDir, pid)))
	}
	send(done)
}

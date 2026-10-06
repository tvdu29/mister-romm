package app

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type dlItem struct {
	id, pid, name, dest, sha1 string
	size                      int64
}

type dlStart struct{ idx int }
type dlProgress struct {
	idx                int
	done, total, speed int64
	phase              string
}
type dlDoneMsg struct {
	idx       int
	err       string
	cancelled bool
}

// isArchive: RomM's hash of an archive is computed on its members, not on the file we download.
func isArchive(name string) bool {
	n := strings.ToLower(name)
	for _, ext := range []string{".zip", ".7z", ".rar", ".tar", ".tgz", ".tar.gz", ".tar.bz2"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

// runDownloads downloads the items one after the other. A game is written to <name>.part and only
// renamed when its size (and hash, when checkable) is right, so an interrupted download resumes
// next time and a half file never looks like a game.
func runDownloads(ctx context.Context, e env, items []dlItem, send func(tea.Msg)) {
	for i, it := range items {
		if ctx.Err() != nil {
			send(dlDoneMsg{idx: i, err: "cancelled", cancelled: true})
			continue
		}
		send(dlStart{idx: i})
		err := downloadOne(ctx, e, it, func(p dlProgress) { p.idx = i; send(p) })
		switch {
		case err == nil:
			send(dlDoneMsg{idx: i})
		case ctx.Err() != nil:
			send(dlDoneMsg{idx: i, err: "cancelled", cancelled: true})
		default:
			logError(e.cfgDir, "download "+it.name+": "+err.Error())
			send(dlDoneMsg{idx: i, err: err.Error()})
		}
	}
}

func downloadOne(ctx context.Context, e env, it dlItem, progress func(dlProgress)) error {
	if !safeName(it.name) || !within(e.gamesDir, it.dest) {
		return fmt.Errorf("refusing to write %q: it is not inside the games folder", it.dest)
	}
	if err := os.MkdirAll(filepath.Dir(it.dest), 0o755); err != nil {
		return err
	}
	part := it.dest + ".part"
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	if have > it.size {
		_ = os.Remove(part)
		have = 0
	}
	if have < it.size {
		for attempt := 0; attempt < 2; attempt++ {
			retry, err := fetchInto(ctx, e, it, part, have, progress)
			if err != nil {
				return err
			}
			if !retry {
				break
			}
			_ = os.Remove(part) // the server could not resume: start again from zero
			have = 0
		}
	}
	st, err := os.Stat(part)
	if err != nil {
		return err
	}
	if st.Size() != it.size {
		return fmt.Errorf("unexpected size: got %d, expected %d", st.Size(), it.size)
	}
	if it.sha1 != "" && !isArchive(it.name) {
		progress(dlProgress{done: it.size, total: it.size, phase: "verifying"})
		sum, err := fileSHA1(ctx, part)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, it.sha1) {
			_ = os.Remove(part)
			return fmt.Errorf("hash mismatch (the file was discarded)")
		}
	}
	return os.Rename(part, it.dest)
}

// fetchInto downloads (or resumes) into part. It reports retry=true when it must start over.
func fetchInto(ctx context.Context, e env, it dlItem, part string, have int64, progress func(dlProgress)) (retry bool, err error) {
	hdr := map[string]string{}
	if have > 0 {
		hdr["Range"] = fmt.Sprintf("bytes=%d-", have)
	}
	resp, err := e.get(ctx, e.longClient, "/api/roms/"+it.id+"/content/"+url.PathEscape(it.name), hdr)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	flags := os.O_WRONLY | os.O_CREATE
	switch resp.StatusCode {
	case http.StatusOK:
		have, flags = 0, flags|os.O_TRUNC // the server ignored the range
	case http.StatusPartialContent:
		var from int64 = -1
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-", &from); err != nil || from != have {
			return true, nil // not the part we asked for: start again from zero
		}
		flags |= os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		return have != it.size, nil
	default:
		return false, fmt.Errorf("%s", explainHTTP(resp))
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return false, err
	}
	buf := make([]byte, 64*1024)
	done, lastSent, lastBytes := have, time.Now(), have
	// never accept more than what the catalog announced (+1 byte to detect an overflow): a hostile
	// server cannot fill the card by streaming forever
	body := io.LimitReader(resp.Body, it.size-have+1)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return false, werr
			}
			done += int64(n)
			if done > it.size {
				f.Close()
				return false, fmt.Errorf("the server sent more data than announced (%d bytes)", it.size)
			}
		}
		if now := time.Now(); now.Sub(lastSent) >= 400*time.Millisecond {
			speed := int64(float64(done-lastBytes) / now.Sub(lastSent).Seconds())
			progress(dlProgress{done: done, total: it.size, speed: speed, phase: "downloading"})
			lastSent, lastBytes = now, done
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return false, rerr
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	progress(dlProgress{done: done, total: it.size, phase: "downloading"})
	return false, f.Close()
}

func fileSHA1(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	buf := make([]byte, 256*1024)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

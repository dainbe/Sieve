package indexer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	parserReleaseBase = "https://github.com/dainbe/Sieve/releases/latest/download"
	fetchTimeout      = 60 * time.Second
)

var parserLangs = []string{"python", "typescript", "javascript", "rust"}

// FetchMissingParsers downloads any absent {lang}.wasm files into dir.
// Returns the list of languages successfully fetched and any per-language errors.
// Files that already exist are skipped without error.
func FetchMissingParsers(ctx context.Context, dir string) (fetched []string, errs []string) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, []string{fmt.Sprintf("create parsers dir: %v", err)}
	}

	client := &http.Client{Timeout: fetchTimeout}

	for _, lang := range parserLangs {
		dest := filepath.Join(dir, lang+".wasm")
		if _, err := os.Stat(dest); err == nil {
			slog.Debug("fetch-parsers: already present, skipping", "lang", lang)
			continue
		}

		url := parserReleaseBase + "/" + lang + ".wasm"
		slog.Info("fetch-parsers: downloading", "lang", lang, "url", url)

		if err := downloadFile(ctx, client, url, dest); err != nil {
			slog.Warn("fetch-parsers: failed", "lang", lang, "err", err)
			errs = append(errs, fmt.Sprintf("%s: %v", lang, err))
			continue
		}

		slog.Info("fetch-parsers: done", "lang", lang)
		fetched = append(fetched, lang)
	}
	return fetched, errs
}

func downloadFile(ctx context.Context, client *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()        //nolint:errcheck
		os.Remove(tmp)   //nolint:errcheck
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}

	return os.Rename(tmp, dest)
}

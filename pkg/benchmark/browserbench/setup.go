package browserbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// DefaultRoot is the conventional local cache for browserbench datasets.
const DefaultRoot = ".cache/browserbench"

// maxDatasetBytes bounds the download; the pinned dev file is 36 KB.
const maxDatasetBytes = 64 << 20

// Setup places the pinned dataset file under root. An existing file is
// accepted only when its digest matches the pin; otherwise the file is
// downloaded from the Hugging Face repository at the pinned revision,
// checked, and installed atomically.
func Setup(ctx context.Context, root string, dataset Dataset, client *http.Client) error {
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) {
		return fmt.Errorf("unsafe browserbench setup root %q", root)
	}
	if dataset.RepositoryURL == "" || dataset.Revision == "" || dataset.File == "" || dataset.SHA256 == "" {
		return errors.New("browserbench dataset pin is required")
	}
	destination := dataset.Path(root)
	if _, err := os.Stat(destination); err == nil {
		_, err := loadRows(destination, dataset.SHA256)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect AssistantBench file: %w", err)
	}
	source, err := url.JoinPath(dataset.RepositoryURL, "resolve", dataset.Revision, dataset.File)
	if err != nil {
		return fmt.Errorf("build AssistantBench download URL: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return fmt.Errorf("build AssistantBench download: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download AssistantBench file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download AssistantBench file %s: HTTP %d", source, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDatasetBytes+1))
	if err != nil {
		return fmt.Errorf("download AssistantBench file: %w", err)
	}
	if len(data) > maxDatasetBytes {
		return fmt.Errorf("AssistantBench file exceeds %d bytes", maxDatasetBytes)
	}
	digest := sha256.Sum256(data)
	if got := hex.EncodeToString(digest[:]); got != dataset.SHA256 {
		return fmt.Errorf("downloaded AssistantBench file has SHA-256 %s; want pinned %s", got, dataset.SHA256)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create AssistantBench cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".download-")
	if err != nil {
		return fmt.Errorf("create AssistantBench temporary file: %w", err)
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write AssistantBench file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write AssistantBench file: %w", err)
	}
	if err := os.Chmod(temporary.Name(), 0o644); err != nil {
		return fmt.Errorf("write AssistantBench file: %w", err)
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		return fmt.Errorf("install AssistantBench file: %w", err)
	}
	_, err = loadRows(destination, dataset.SHA256)
	return err
}

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	sandbox "github.com/geminixiang/agent-sandbox-platform/packages/sdk-go"
)

const streamBytes = 10 * 1024 * 1024

type coverage struct {
	Pool   string `json:"pool"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var calls int
	client, err := sandbox.NewClient(sandbox.ClientOptions{BaseURL: required("SANDBOX_PLATFORM_URL"), Credentials: sandbox.TokenProviderFunc(func(context.Context) (string, error) { calls++; return required("SANDBOX_SUBJECT_TOKEN"), nil })})
	must(err)
	defer client.Close()
	var results []coverage
	for _, pool := range []string{"coding", "browser"} {
		results = append(results, verifyPool(ctx, client, pool))
		waitReady(ctx)
	}
	if calls < 15 {
		panic(fmt.Sprintf("token provider calls=%d", calls))
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "passed", "tokenProviderCalls": calls, "streaming": results})
}

func verifyPool(ctx context.Context, client *sandbox.Client, pool string) coverage {
	box, err := client.Create(ctx, sandbox.CreateOptions{Pool: pool, TTLSeconds: 300, IdempotencyKey: fmt.Sprintf("go-sdk-%s-%d", pool, time.Now().UnixNano())})
	must(err)
	released := false
	defer func() {
		if !released {
			_ = box.Close(context.Background())
		}
	}()
	must(box.Files().WriteText(ctx, "/workspace/go-sdk.txt", "hello-"+pool))
	text, err := box.Files().ReadText(ctx, "/workspace/go-sdk.txt")
	must(err)
	if text != "hello-"+pool {
		panic("text mismatch")
	}
	must(box.Files().WriteBytes(ctx, "/workspace/go-sdk.bin", []byte{0, 255, 1, 128}))
	value, err := box.Files().ReadBytes(ctx, "/workspace/go-sdk.bin")
	must(err)
	if !bytes.Equal(value, []byte{0, 255, 1, 128}) {
		panic("bytes mismatch")
	}
	result, err := box.Run(ctx, "printf out; printf err >&2", sandbox.RunOptions{Check: true})
	must(err)
	if result.Stdout != "out" || result.Stderr != "err" {
		panic("run mismatch")
	}
	_, err = box.Run(ctx, "printf kept-out; printf kept-err >&2; exit 19", sandbox.RunOptions{Check: true})
	var commandErr *sandbox.CommandFailedError
	if !errors.As(err, &commandErr) || commandErr.Result.ExitCode != 19 || commandErr.Result.Stderr != "kept-err" {
		panic("checked error mismatch")
	}
	pager := client.Pager(sandbox.ListOptions{Pool: pool, Limit: 1})
	found := false
	for pager.Next(ctx) {
		found = found || pager.Sandbox().ID() == box.ID()
	}
	must(pager.Err())
	if !found {
		panic("list omitted sandbox")
	}
	connected, err := client.Connect(ctx, box.ID())
	must(err)
	persisted, err := connected.Files().ReadText(ctx, "/workspace/go-sdk.txt")
	must(err)
	if persisted != "hello-"+pool {
		panic("persistence mismatch")
	}
	payload := bytes.Repeat([]byte("go-sdk-stream|"), streamBytes/14+1)[:streamBytes]
	digest := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(digest[:])
	must(connected.Files().WriteStream(ctx, "/workspace/go-stream.bin", bytes.NewReader(payload), sandbox.UploadOptions{SizeBytes: int64(len(payload)), SHA256: digestHex}))
	download, err := connected.Files().ReadStream(ctx, "/workspace/go-stream.bin")
	must(err)
	read, err := io.ReadAll(download.Content)
	must(err)
	if !bytes.Equal(read, payload) || download.SHA256 != digestHex {
		panic("stream mismatch")
	}
	if pool == "browser" {
		result, err = connected.Run(ctx, "node /opt/browser/smoke.mjs", sandbox.RunOptions{Check: true, TimeoutSeconds: 60})
		must(err)
		if result.ExitCode != 0 {
			panic("browser failed")
		}
	}
	record, err := box.Release(ctx)
	must(err)
	released = true
	if record.Status != sandbox.LeaseReleased {
		panic("release mismatch")
	}
	_, err = client.Connect(ctx, box.ID())
	if !errors.Is(err, sandbox.ErrNotFound) {
		panic("released sandbox visible")
	}
	return coverage{Pool: pool, Bytes: len(payload), SHA256: digestHex}
}

func waitReady(ctx context.Context) {
	url := required("SANDBOX_PLATFORM_URL") + "/ready"
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		select {
		case <-ctx.Done():
			panic(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		panic(name + " is required")
	}
	return value
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}

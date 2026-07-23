package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type Files struct{ sandbox *Sandbox }

type fileReadResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (f *Files) ReadText(ctx context.Context, path string) (string, error) {
	content, encoding, err := f.read(ctx, path, "utf8")
	if err != nil {
		return "", err
	}
	if encoding != "utf8" {
		return "", &Error{Code: codeInvalidResponse, Message: "sandbox: platform returned an unexpected file encoding"}
	}
	return content, nil
}
func (f *Files) WriteText(ctx context.Context, path, content string) error {
	return f.write(ctx, path, content, "utf8")
}
func (f *Files) ReadBytes(ctx context.Context, path string) ([]byte, error) {
	content, encoding, err := f.read(ctx, path, "base64")
	if err != nil {
		return nil, err
	}
	if encoding != "base64" {
		return nil, &Error{Code: codeInvalidResponse, Message: "sandbox: platform returned an unexpected file encoding"}
	}
	value, err := base64.StdEncoding.Strict().DecodeString(content)
	if err != nil || base64.StdEncoding.EncodeToString(value) != content {
		return nil, integrityError("sandbox: platform returned non-canonical base64", err)
	}
	return value, nil
}
func (f *Files) WriteBytes(ctx context.Context, path string, content []byte) error {
	return f.write(ctx, path, base64.StdEncoding.EncodeToString(content), "base64")
}
func (f *Files) read(ctx context.Context, path, encoding string) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", errors.New("sandbox: path must not be blank")
	}
	var response fileReadResponse
	err := f.sandbox.client.doJSON(ctx, http.MethodPost, leasePath+"/"+url.PathEscape(f.sandbox.ID())+"/files/read", nil, nil, map[string]string{"path": path, "encoding": encoding}, &response, http.StatusOK)
	return response.Content, response.Encoding, err
}
func (f *Files) write(ctx context.Context, path, content, encoding string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("sandbox: path must not be blank")
	}
	return f.sandbox.client.doJSON(ctx, http.MethodPost, leasePath+"/"+url.PathEscape(f.sandbox.ID())+"/files/write", nil, nil, map[string]string{"path": path, "content": content, "encoding": encoding}, nil, http.StatusOK)
}

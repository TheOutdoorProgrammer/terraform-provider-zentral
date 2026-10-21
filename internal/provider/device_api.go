package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/zentralopensource/goztl"
)

// API errors can contain reflected payloads and identifiers. Keep these out of diagnostics.
func deviceRequest(ctx context.Context, client *goztl.Client, method, path string, body, out any) (int, error) {
	req, err := client.NewRequest(ctx, method, path, body)
	if err != nil {
		return 0, errors.New("could not construct Zentral API request")
	}
	resp, err := client.Do(ctx, req, out)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		return status, fmt.Errorf("Zentral API request failed (HTTP status %d); inspect the server's protected audit log", status)
	}
	return status, nil
}

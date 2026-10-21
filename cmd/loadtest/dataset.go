package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxDatasetBody bounds how much of a task-list response is decoded while
// checking the dataset size. The listing itself is never printed.
const maxDatasetBody = 4 << 20

// dataset is the result of preparing test data: the identifiers created by this
// process and the bookkeeping needed to remove exactly those rows again.
type dataset struct {
	Marker   string
	IDs      []string
	Baseline int
	Total    int
	Took     time.Duration
}

// seedTasks creates n tasks with a run-specific title marker and a shared
// "loadtest" assignee, so leftovers from an interrupted run are identifiable
// without being deleted by a later one. It returns only the IDs this call
// created; cleanup never touches anything else.
func seedTasks(ctx context.Context, client *http.Client, base *url.URL, n int, marker string) (dataset, error) {
	ds := dataset{Marker: marker}

	baseline, err := fetchTotal(ctx, client, base)
	if err != nil {
		return ds, fmt.Errorf("read dataset size before seeding: %w", err)
	}
	ds.Baseline = baseline

	createURL := *base
	createURL.Path = "/task"

	start := time.Now()
	ds.IDs = make([]string, 0, n)
	for i := 0; i < n; i++ {
		body, err := json.Marshal(map[string]string{
			"title":    fmt.Sprintf("%s-%d", marker, i),
			"status":   "todo",
			"assignee": "loadtest",
		})
		if err != nil {
			return ds, fmt.Errorf("encode task: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL.String(), bytes.NewReader(body))
		if err != nil {
			return ds, fmt.Errorf("build create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return ds, fmt.Errorf("create task %d: %w", i, err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxDatasetBody))
		_ = resp.Body.Close()
		if readErr != nil {
			return ds, fmt.Errorf("create task %d: read body: %w", i, readErr)
		}
		if resp.StatusCode != http.StatusCreated {
			return ds, fmt.Errorf("create task %d: status %d", i, resp.StatusCode)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(payload, &created); err != nil || created.ID == "" {
			return ds, fmt.Errorf("create task %d: response has no id", i)
		}
		ds.IDs = append(ds.IDs, created.ID)
	}
	ds.Took = time.Since(start)

	total, err := fetchTotal(ctx, client, base)
	if err != nil {
		return ds, fmt.Errorf("read dataset size after seeding: %w", err)
	}
	ds.Total = total
	if want := ds.Baseline + n; ds.Total != want {
		return ds, fmt.Errorf("dataset size after seeding is %d, want %d (baseline %d + %d)", ds.Total, want, ds.Baseline, n)
	}
	return ds, nil
}

// cleanupTasks deletes the tasks this run created. A 404 is accepted because a
// row may already be gone; every other failure is reported.
func cleanupTasks(ctx context.Context, client *http.Client, base *url.URL, ids []string) (deleted, failed int, err error) {
	deleteURL := *base
	for _, id := range ids {
		u := deleteURL
		u.Path = "/tasks/" + url.PathEscape(id)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
		if err != nil {
			return deleted, len(ids) - deleted, fmt.Errorf("build delete request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return deleted, len(ids) - deleted, fmt.Errorf("delete %s: %w", id, err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDatasetBody))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNoContent, http.StatusOK:
			deleted++
		case http.StatusNotFound:
			deleted++
		default:
			failed++
		}
	}
	if failed > 0 {
		return deleted, failed, fmt.Errorf("%d of %d tasks could not be deleted", failed, len(ids))
	}
	return deleted, 0, nil
}

// fetchTotal walks the unfiltered cursor-paginated list and counts items, so
// dataset bookkeeping exercises the same contract under test.
func fetchTotal(ctx context.Context, client *http.Client, base *url.URL) (int, error) {
	total := 0
	cursor := ""
	for {
		u := *base
		u.Path = "/tasks"
		q := url.Values{}
		q.Set("page_size", "100")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return 0, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		payload, err := io.ReadAll(io.LimitReader(resp.Body, maxDatasetBody))
		_ = resp.Body.Close()
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("status %d", resp.StatusCode)
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor *string           `json:"next_cursor"`
			HasMore    bool              `json:"has_more"`
		}
		if err := json.Unmarshal(payload, &page); err != nil {
			return 0, fmt.Errorf("decode list response: %w", err)
		}
		total += len(page.Items)
		if !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			return total, nil
		}
		cursor = *page.NextCursor
	}
}

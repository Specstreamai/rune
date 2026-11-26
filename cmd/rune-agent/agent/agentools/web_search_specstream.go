// Copyright (C) 2017-2026 The Rune Authors
// SPDX-License-Identifier: GPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at
// your option) any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
// General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package agentools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	specStreamBaseURL = "https://api.specstream.ai"
	// specStreamSnippetMaxChars asks SpecStream for one best-matching
	// excerpt per page cut to the length the tool keeps. SpecStream counts
	// characters, so the tool still trims non-ASCII text past the byte cap.
	specStreamSnippetMaxChars = webSearchMaxSnippetBytes
	// specStreamMaxResponseBytes bounds the response body read into memory.
	specStreamMaxResponseBytes = 1 << 20
)

// SpecStreamSearch is a WebSearchProvider backed by SpecStream's search API
// (https://specstream.ai/docs). It needs an API key.
type SpecStreamSearch struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

var _ WebSearchProvider = (*SpecStreamSearch)(nil)

// NewSpecStreamSearch returns a SpecStream provider that authenticates
// with apiKey.
func NewSpecStreamSearch(apiKey string) *SpecStreamSearch {
	return &SpecStreamSearch{
		apiKey:  apiKey,
		baseURL: specStreamBaseURL,
		client:  http.DefaultClient,
	}
}

type specStreamSearchRequest struct {
	Q              string `json:"q"`
	Num            int    `json:"num,omitempty"`
	Compact        bool   `json:"compact"`
	SourceMaxChars int    `json:"source_max_chars"`
}

type specStreamSearchResponse struct {
	Data struct {
		Sources []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"sources"`
	} `json:"data"`
}

type specStreamError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Name satisfies WebSearchProvider.
func (s *SpecStreamSearch) Name() string { return "SpecStream" }

// Search satisfies WebSearchProvider.
func (s *SpecStreamSearch) Search(ctx context.Context, q WebSearchQuery) ([]WebSearchResult, error) {
	if s.apiKey == "" {
		return nil, errors.New("SPECSTREAM_API_KEY is not set")
	}
	query := q.Query
	if q.Site != "" {
		// SpecStream applies site: to every result rather than treating it
		// as a ranking hint.
		query += " site:" + q.Site
	}
	body, err := json.Marshal(specStreamSearchRequest{
		Q:              query,
		Num:            q.MaxResults,
		Compact:        true,
		SourceMaxChars: specStreamSnippetMaxChars,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/web-search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Read one byte past the limit to tell a full response from a cut one.
	data, err := io.ReadAll(io.LimitReader(resp.Body, specStreamMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, specStreamStatusError(resp.StatusCode, data)
	}
	if len(data) > specStreamMaxResponseBytes {
		return nil, fmt.Errorf("response is larger than %d bytes", specStreamMaxResponseBytes)
	}

	var parsed specStreamSearchResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	results := make([]WebSearchResult, 0, len(parsed.Data.Sources))
	for _, r := range parsed.Data.Sources {
		if r.URL == "" {
			continue
		}
		results = append(results, WebSearchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return results, nil
}

func specStreamStatusError(status int, body []byte) error {
	var e specStreamError
	_ = json.Unmarshal(body, &e)
	msg := fmt.Sprintf("HTTP %d", status)
	if detail := strings.TrimSpace(e.Error.Message); detail != "" {
		msg += ": " + detail
	}
	switch status {
	case http.StatusUnauthorized:
		msg += " (check SPECSTREAM_API_KEY)"
	case http.StatusPaymentRequired:
		msg += " (the account's balance is used up)"
	}
	return errors.New(msg)
}

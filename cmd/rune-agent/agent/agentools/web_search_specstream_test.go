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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// specStreamServer serves handler and returns a provider pointed at it.
func specStreamServer(t *testing.T, apiKey string, handler http.HandlerFunc) *SpecStreamSearch {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := NewSpecStreamSearch(apiKey)
	s.baseURL = srv.URL
	s.client = srv.Client()
	return s
}

func TestSpecStreamSearchRequest(t *testing.T) {
	tests := []struct {
		name   string
		query  WebSearchQuery
		wantQ  string
		wantNo float64
	}{
		{name: "plain query", query: WebSearchQuery{Query: "go generics", MaxResults: 3}, wantQ: "go generics", wantNo: 3},
		{name: "site becomes an operator", query: WebSearchQuery{Query: "WithTimeout", MaxResults: 5, Site: "pkg.go.dev"}, wantQ: "WithTimeout site:pkg.go.dev", wantNo: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod, gotPath, gotAuth, gotType string
			var gotBody map[string]any
			s := specStreamServer(t, "ss-test", func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotType = r.Header.Get("Content-Type")
				data, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(data, &gotBody)
				_, _ = io.WriteString(w, `{"data":{"sources":[]}}`)
			})
			_, err := s.Search(t.Context(), tt.query)
			require.NoError(t, err)
			assert.Equal(t, http.MethodPost, gotMethod)
			assert.Equal(t, "/v1/web-search", gotPath)
			assert.Equal(t, "Bearer ss-test", gotAuth)
			assert.Equal(t, "application/json", gotType)
			assert.Equal(t, map[string]any{
				"q":                tt.wantQ,
				"num":              tt.wantNo,
				"compact":          true,
				"source_max_chars": float64(webSearchMaxSnippetBytes),
			}, gotBody)
		})
	}
}

func TestSpecStreamSearchWithoutAKeySendsNothing(t *testing.T) {
	called := false
	s := specStreamServer(t, "", func(http.ResponseWriter, *http.Request) { called = true })
	_, err := s.Search(t.Context(), WebSearchQuery{Query: "q", MaxResults: 5})
	require.ErrorContains(t, err, "SPECSTREAM_API_KEY is not set")
	assert.False(t, called)
}

func TestSpecStreamSearchMapsResults(t *testing.T) {
	s := specStreamServer(t, "ss-test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"query":"q","kind":"list","answer":null,"rows":[],"sources":[
			{"title":"Go","url":"https://go.dev","published":"2026-10-01","snippet":"Build simple,\nsecure software."},
			{"title":"No URL","url":"","snippet":"dropped"}
		],"took_ms":812}}`)
	})
	results, err := s.Search(t.Context(), WebSearchQuery{Query: "q", MaxResults: 5})
	require.NoError(t, err)
	assert.Equal(t, []WebSearchResult{
		{Title: "Go", URL: "https://go.dev", Snippet: "Build simple,\nsecure software."},
	}, results)
}

func TestSpecStreamSearchErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{
			name:    "bad request carries the API's message",
			status:  http.StatusUnprocessableEntity,
			body:    `{"error":{"code":"invalid_request","message":"num must be between 1 and 100","details":{}}}`,
			wantErr: "HTTP 422: num must be between 1 and 100",
		},
		{
			name:    "rejected key",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"code":"invalid_token","message":"SpecStream didn't accept this API key or token.","details":{}}}`,
			wantErr: "HTTP 401: SpecStream didn't accept this API key or token. (check SPECSTREAM_API_KEY)",
		},
		{
			name:    "empty balance",
			status:  http.StatusPaymentRequired,
			body:    `{"error":{"code":"insufficient_balance","message":"","details":{}}}`,
			wantErr: "HTTP 402 (the account's balance is used up)",
		},
		{
			name:    "rate limit",
			status:  http.StatusTooManyRequests,
			body:    `{"error":{"code":"rate_limited","message":"Too many requests","details":{}}}`,
			wantErr: "HTTP 429: Too many requests",
		},
		{
			name:    "body that is not JSON",
			status:  http.StatusBadGateway,
			body:    `<html>bad gateway</html>`,
			wantErr: "HTTP 502",
		},
		{
			name:    "malformed success",
			status:  http.StatusOK,
			body:    `{"data":`,
			wantErr: "parse response",
		},
		{
			name:    "response over the size limit",
			status:  http.StatusOK,
			body:    `{"data":{"sources":[` + strings.Repeat(" ", specStreamMaxResponseBytes) + `]}}`,
			wantErr: "response is larger than 1048576 bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := specStreamServer(t, "ss-test", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			_, err := s.Search(t.Context(), WebSearchQuery{Query: "q", MaxResults: 5})
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestSpecStreamSearchHonorsContext(t *testing.T) {
	started := make(chan struct{})
	s := specStreamServer(t, "ss-test", func(_ http.ResponseWriter, r *http.Request) {
		// The server notices a dropped connection, and cancels the
		// request's context, only once the body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := s.Search(ctx, WebSearchQuery{Query: "q", MaxResults: 5})
		errc <- err
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the server")
	}
	cancel()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("Search did not return after the context was canceled")
	}
}

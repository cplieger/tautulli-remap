// Package plex adapts the shared github.com/cplieger/plexapi/v2 client to the
// remap workflow's types. The HTTP transport — token-in-header-only,
// redirect refusal, same-origin path guard, transparent retry of transient
// failures, bounded reads — is the library's; this package owns only the
// mapping into remap.Section / remap.LibItem, the GUID normalization, and
// the workflow's fail-closed rules.
package plex

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/cplieger/plexapi/v2"
	"github.com/cplieger/runesafe/v3"
	"github.com/cplieger/tautulli-remap/internal/remap"
)

// Client is the remap-facing Plex API client.
type Client struct {
	api *plexapi.Client
}

// Token is plexapi's Plex-token type, re-exported so a caller constructing a
// client through this wrapper does not import plexapi for one conversion.
type Token = plexapi.Token

// New builds the client for the given server URL and token. The library
// validates the URL and installs the hardened transport (refuse-all
// redirects, header-borne token, retry with Retry-After honoring).
func New(plexURL string, token plexapi.Token) (*Client, error) {
	api, err := plexapi.New(plexURL, token)
	if err != nil {
		return nil, fmt.Errorf("plex client: %w", err)
	}
	return &Client{api: api}, nil
}

// ItemExists reports whether a Plex library item with the given rating key
// currently exists. A non-numeric key is treated as not-exists (false, nil).
// A 200 means it exists, a 404 means it definitively does not; any other
// failure returns an error — callers must fail closed rather than treat the
// item as stale.
func (c *Client) ItemExists(ctx context.Context, ratingKey string) (bool, error) {
	if !remap.RatingKey(ratingKey).IsValid() {
		slog.Warn("invalid rating key (non-numeric)", "key", ratingKey)
		return false, nil
	}
	return c.api.ItemExists(ctx, plexapi.RatingKey(ratingKey))
}

// LibrarySections returns all library sections. A non-nil error means the
// list could not be fetched; callers treat that as a degraded Plex rather
// than an empty library.
func (c *Client) LibrarySections(ctx context.Context) ([]remap.Section, error) {
	sections, err := c.api.Sections(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch Plex sections: %w", err)
	}
	out := make([]remap.Section, 0, len(sections))
	for _, s := range sections {
		out = append(out, remap.Section{Key: s.Key, Title: runesafe.Untrusted(s.Title), Type: s.Type})
	}
	return out, nil
}

// LibraryAll returns all items in the identified section, with rating keys
// parsed and GUIDs normalized (unsupported formats dropped, entries with a
// non-numeric rating key skipped with a warning). A non-nil error means the
// section could not be fetched; callers must not mistake a partial outage
// for missing content.
func (c *Client) LibraryAll(ctx context.Context, sectionKey string) ([]remap.LibItem, error) {
	if !remap.RatingKey(sectionKey).IsValid() {
		return nil, fmt.Errorf("invalid section key (non-numeric): %q", sectionKey)
	}
	items, err := c.api.SectionItems(ctx, plexapi.RatingKey(sectionKey))
	if err != nil {
		return nil, fmt.Errorf("fetch Plex library items: %w", err)
	}
	out := make([]remap.LibItem, 0, len(items))
	for i := range items {
		m := &items[i]
		rk, err := strconv.Atoi(m.RatingKey)
		if err != nil {
			slog.Warn("invalid rating key", "key", m.RatingKey, "title", runesafe.Sanitize(m.Title))
			continue
		}
		var guids []string
		if g := remap.NormalizeGUID(m.GUID); g != "" {
			guids = append(guids, g)
		}
		for _, g := range m.GUIDs {
			if ng := remap.NormalizeGUID(g.ID); ng != "" {
				guids = append(guids, ng)
			}
		}
		out = append(out, remap.LibItem{
			RatingKey: rk, Title: runesafe.Untrusted(m.Title), Year: m.Year, GUIDs: guids,
		})
	}
	return out, nil
}

// ResolveEpisodeShow resolves a Plex episode GUID (plex://episode/<hash>)
// to the rating key of the show that currently contains it. Returns ("", nil)
// when the GUID matches nothing or is ambiguous (multiple shows); a non-nil
// error means the lookup could not be completed.
func (c *Client) ResolveEpisodeShow(ctx context.Context, episodeGUID string) (string, error) {
	show, err := c.api.ShowForEpisodeGUID(ctx, episodeGUID)
	if err != nil {
		return "", fmt.Errorf("plex guid resolve: %w", err)
	}
	return show, nil
}

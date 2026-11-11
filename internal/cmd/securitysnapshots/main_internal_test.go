package main

// White-box testing required: the source updater's review decision
// is private to this command.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/vulnerability"
)

func testSnapshot(t *testing.T, source []byte, now time.Time) (string, snapshot) {
	t.Helper()
	var current snapshot
	if err := json.Unmarshal(vulnerability.Snapshot(), &current); err != nil {
		t.Fatal(err)
	}
	content, err := advisoryContent(source)
	if err != nil {
		t.Fatal(err)
	}
	rawDigest, contentDigest := sha256.Sum256(source), sha256.Sum256(content)
	current.SourceSHA256 = hex.EncodeToString(rawDigest[:])
	current.AdvisoryContentSHA256 = hex.EncodeToString(contentDigest[:])
	current.RetrievedAt = now
	path := filepath.Join(t.TempDir(), "ydb.json")
	if err := writeJSON(path, current); err != nil {
		t.Fatal(err)
	}
	return path, current
}

func TestDocumentationRevisionDoesNotCreateReview(t *testing.T) {
	c := testcheck.New(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	before := []byte("---\nrevision: first\n---\n# Security changelog\nCVE-2026-10549\n")
	after := []byte("---\nrevision: second\n---\n# Security changelog\nCVE-2026-10549\n")
	path, current := testSnapshot(t, before, now)
	c.Assert(update(path, after, now.Add(time.Hour)), qt.IsNil)
	data, err := os.ReadFile(path)
	c.Assert(err, qt.IsNil)
	var retained snapshot
	c.Assert(json.Unmarshal(data, &retained), qt.IsNil)
	c.Assert(retained.SourceSHA256, qt.Equals, current.SourceSHA256)
	c.Assert(retained.RetrievedAt, qt.Equals, current.RetrievedAt)
	c.Assert(update(path, after, now.Add(refreshInterval)), qt.IsNil)
	data, err = os.ReadFile(path)
	c.Assert(err, qt.IsNil)
	c.Assert(json.Unmarshal(data, &retained), qt.IsNil)
	digest := sha256.Sum256(after)
	c.Assert(retained.SourceSHA256, qt.Equals, hex.EncodeToString(digest[:]))
	c.Assert(retained.RetrievedAt, qt.Equals, now.Add(refreshInterval))
	_, err = os.Stat(filepath.Join(filepath.Dir(path), "review", "ydb.json"))
	c.Assert(err, qt.ErrorIs, os.ErrNotExist)
}

func TestChangedAdvisoryContentNeedsReview(t *testing.T) {
	c := testcheck.New(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	before := []byte("---\nrevision: first\n---\n# Security changelog\nCVE-2026-10549\nCVE-2022-28228\nCVE-2023-45825\n")
	changed := []byte("---\nrevision: second\n---\n# Security changelog\nCVE-2026-10549\nCVE-2023-45825\nCVE-2027-12345\nCVE-2027-12345\n")
	path, current := testSnapshot(t, before, now)
	c.Assert(update(path, changed, now.Add(time.Hour)), qt.IsNil)
	reviewPath := filepath.Join(filepath.Dir(path), "review", "ydb.json")
	data, err := os.ReadFile(reviewPath)
	c.Assert(err, qt.IsNil)
	var pending review
	c.Assert(json.Unmarshal(data, &pending), qt.IsNil)
	c.Assert(pending.AddedIDs, qt.DeepEquals, []string{"CVE-2027-12345"})
	c.Assert(pending.MissingIDs, qt.DeepEquals, []string{"CVE-2022-28228"})
	c.Assert(pending.ReviewedSHA, qt.Equals, current.SourceSHA256)
	c.Assert(update(path, changed, now.Add(2*time.Hour)), qt.IsNil)
	again, err := os.ReadFile(reviewPath)
	c.Assert(err, qt.IsNil)
	c.Assert(again, qt.DeepEquals, data)
	c.Assert(update(path, before, now.Add(3*time.Hour)), qt.IsNil)
	_, err = os.Stat(reviewPath)
	c.Assert(err, qt.ErrorIs, os.ErrNotExist)
}

func TestChangedSourceWithoutAdvisoriesIsRefused(t *testing.T) {
	c := testcheck.New(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	before := []byte("---\nrevision: first\n---\n# Security changelog\nCVE-2026-10549\n")
	path, _ := testSnapshot(t, before, now)
	c.Assert(update(path, []byte("---\nrevision: second\n---\n# Security changelog\nNo advisory records\n"), now), qt.ErrorMatches, "changed source contains no CVE IDs")
	c.Assert(update(path, []byte("missing heading"), now), qt.ErrorMatches, "YDB security changelog heading missing")
}

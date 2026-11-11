// Command securitysnapshots checks the provider's reviewed YDB server
// advisories against the official changelog. Changed ranges need human review.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

const (
	sourceURL       = "https://ydb.tech/docs/en/security-changelog.md"
	snapshotPath    = "internal/vulnerability/ydb.json"
	sourceLimit     = 8 << 20
	refreshInterval = 14 * 24 * time.Hour
)

var cveID = regexp.MustCompile(`CVE-[0-9]{4}-[0-9]{4,}`)

type snapshot struct {
	SchemaVersion         int       `json:"schema_version"`
	Product               string    `json:"product"`
	Source                string    `json:"source"`
	RetrievedAt           time.Time `json:"retrieved_at"`
	SourceSHA256          string    `json:"source_sha256"`
	AdvisoryContentSHA256 string    `json:"advisory_content_sha256"`
	Coverage              string    `json:"coverage"`
	CoverageNote          string    `json:"coverage_note,omitempty"`
	OutOfScope            []struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	} `json:"out_of_scope,omitempty"`
	Advisories []struct {
		ID          string   `json:"id"`
		Component   string   `json:"component"`
		Description string   `json:"description"`
		URL         string   `json:"url"`
		CVSS        *float64 `json:"cvss,omitempty"`
		Ranges      []struct {
			Line  string `json:"line"`
			From  string `json:"from"`
			Fixed string `json:"fixed"`
		} `json:"ranges"`
	} `json:"advisories"`
}

type review struct {
	SchemaVersion int       `json:"schema_version"`
	Product       string    `json:"product"`
	Source        string    `json:"source"`
	RetrievedAt   time.Time `json:"retrieved_at"`
	ReviewedSHA   string    `json:"reviewed_sha256"`
	ObservedSHA   string    `json:"observed_sha256"`
	ObservedIDs   []string  `json:"observed_cve_ids"`
	AddedIDs      []string  `json:"new_cve_ids"`
	MissingIDs    []string  `json:"missing_cve_ids"`
}

func main() {
	input := flag.String("input", "", "Saved source page for an offline check")
	flag.Parse()
	var source []byte
	var err error
	if *input != "" {
		source, err = os.ReadFile(*input)
	} else {
		source, err = fetch(&http.Client{Timeout: 30 * time.Second})
	}
	if err == nil {
		err = update(snapshotPath, source, time.Now().UTC())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ydb:", err)
		os.Exit(1)
	}
}

func fetch(client *http.Client) ([]byte, error) {
	response, err := client.Get(sourceURL) // #nosec G107 -- the URL is the fixed YDB source.
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("advisory source returned HTTP %d", response.StatusCode)
	}
	source, err := io.ReadAll(io.LimitReader(response.Body, sourceLimit+1))
	if err != nil {
		return nil, err
	}
	if len(source) > sourceLimit {
		return nil, errors.New("advisory source exceeds 8 MiB")
	}
	return source, nil
}

func update(path string, source []byte, now time.Time) error {
	if len(source) == 0 || len(source) > sourceLimit {
		return errors.New("empty or oversized source")
	}
	content, err := advisoryContent(source)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read reviewed snapshot: %w", err)
	}
	var current snapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&current); err != nil {
		return fmt.Errorf("decode reviewed snapshot: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("reviewed snapshot contains trailing data")
	}
	if current.SchemaVersion != 2 || current.Product != "ydb" || current.Source != sourceURL || len(current.Advisories) == 0 {
		return errors.New("invalid YDB reviewed snapshot metadata")
	}
	rawDigest := sha256.Sum256(source)
	contentDigest := sha256.Sum256(content)
	rawSHA, contentSHA := hex.EncodeToString(rawDigest[:]), hex.EncodeToString(contentDigest[:])
	reviewPath := filepath.Join(filepath.Dir(path), "review", "ydb.json")
	if rawSHA == current.SourceSHA256 || contentSHA == current.AdvisoryContentSHA256 {
		if err := os.Remove(reviewPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		age := now.Sub(current.RetrievedAt)
		if age >= 0 && age < refreshInterval {
			fmt.Println("ydb: advisory content unchanged; snapshot remains current")
			return nil
		}
		current.RetrievedAt, current.SourceSHA256, current.AdvisoryContentSHA256 = now, rawSHA, contentSHA
		if err := writeJSON(path, current); err != nil {
			return err
		}
		fmt.Println("ydb: refreshed source verification time")
		return nil
	}
	if pendingData, readErr := os.ReadFile(reviewPath); readErr == nil {
		var pending review
		if err := json.Unmarshal(pendingData, &pending); err != nil {
			return fmt.Errorf("decode pending source review: %w", err)
		}
		if pending.ReviewedSHA == current.SourceSHA256 && pending.ObservedSHA == rawSHA {
			fmt.Println("ydb: source review is still pending")
			return nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	report, err := buildReview(current, content, rawSHA, now)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(reviewPath), 0o755); err != nil {
		return err
	}
	if err := writeJSON(reviewPath, report); err != nil {
		return err
	}
	fmt.Printf("ydb: source changed; review %s before updating the snapshot\n", reviewPath)
	return nil
}

func advisoryContent(source []byte) ([]byte, error) {
	const heading = "\n# Security changelog\n"
	index := bytes.Index(source, []byte(heading))
	if index < 0 {
		return nil, errors.New("YDB security changelog heading missing")
	}
	return source[index+1:], nil
}

func buildReview(current snapshot, content []byte, rawSHA string, now time.Time) (review, error) {
	observed := cveID.FindAllString(string(content), -1)
	slices.Sort(observed)
	observed = slices.Compact(observed)
	if len(observed) == 0 {
		return review{}, errors.New("changed source contains no CVE IDs")
	}
	known := make([]string, 0, len(current.Advisories)+len(current.OutOfScope))
	for _, advisory := range current.Advisories {
		known = append(known, advisory.ID)
	}
	for _, excluded := range current.OutOfScope {
		known = append(known, excluded.ID)
	}
	slices.Sort(known)
	return review{SchemaVersion: 1, Product: "ydb", Source: sourceURL, RetrievedAt: now,
		ReviewedSHA: current.SourceSHA256, ObservedSHA: rawSHA, ObservedIDs: observed,
		AddedIDs: difference(observed, known), MissingIDs: difference(known, observed)}, nil
}

func difference(left, right []string) []string {
	var result []string
	for _, id := range left {
		if !slices.Contains(right, id) {
			result = append(result, id)
		}
	}
	return result
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600) // #nosec G703 -- the path is inside the provider checkout.
}

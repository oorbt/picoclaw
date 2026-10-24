package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/utils"
)

const (
	defaultClawHubTimeout  = 30 * time.Second
	defaultMaxZipSize      = 50 * 1024 * 1024 // 50 MB
	defaultMaxResponseSize = 2 * 1024 * 1024  // 2 MB
)

func init() {
	RegisterRegistryProviderBuilder("clawhub", func(_ string, cfg config.SkillRegistryConfig) RegistryProvider {
		privateCfg := clawHubRegistryPrivateConfig{}
		if err := cfg.DecodeParam(&privateCfg); err != nil {
			slog.Warn("invalid clawhub private config", "error", err)
		}
		return ClawHubConfig{
			Enabled:         cfg.Enabled,
			BaseURL:         cfg.BaseURL,
			AuthToken:       cfg.AuthToken.String(),
			SearchPath:      privateCfg.SearchPath,
			SkillsPath:      privateCfg.SkillsPath,
			DownloadPath:    privateCfg.DownloadPath,
			Timeout:         privateCfg.Timeout,
			MaxZipSize:      privateCfg.MaxZipSize,
			MaxResponseSize: privateCfg.MaxResponseSize,
		}
	})
}

type clawHubRegistryPrivateConfig struct {
	SearchPath      string `json:"search_path"`
	SkillsPath      string `json:"skills_path"`
	DownloadPath    string `json:"download_path"`
	Timeout         int    `json:"timeout"`
	MaxZipSize      int    `json:"max_zip_size"`
	MaxResponseSize int    `json:"max_response_size"`
}

// ClawHub owner-scoped skills (verified against the live API, 2026-10):
//   - Slugs are only unique per owner. GET {skills}/{slug} on a slug that several
//     owners publish returns HTTP 409 {"code":"AMBIGUOUS_SKILL_SLUG","matches":[{"ref":"@owner/slug",...}]}.
//   - Meta: GET {skills}/{slug}?ownerHandle={owner} (also accepts ?owner=); unknown
//     owner gives 404. The body is nested: {"skill":{slug,displayName,summary},
//     "latestVersion":{version},"owner":{handle},"moderation":{...}}.
//   - Download: GET {download}?slug={slug}&ownerHandle={owner}[&version=...].
//     A "@owner/slug" or "owner/slug" value in the slug param is a 404.
//   - Search results carry a top-level "ownerHandle" and install.reference
//     ("owner/slug"); we expose "@owner/slug" as SearchResult.Slug.
//
// Identifiers accepted: "@owner/slug", "owner/slug", or a bare "slug". Skills are
// installed into a directory named after the slug alone (matching the SKILL.md
// name); installing two owners' skills with the same slug collides and reports
// "already installed".

// ClawHubRegistry implements SkillRegistry for the ClawHub platform.
type ClawHubRegistry struct {
	baseURL         string
	authToken       string // Optional - for elevated rate limits
	searchPath      string // Search API
	skillsPath      string // For retrieving skill metadata
	downloadPath    string // For fetching ZIP files for download
	maxZipSize      int
	maxResponseSize int
	client          *http.Client
}

// NewClawHubRegistry creates a new ClawHub registry client from config.
func NewClawHubRegistry(cfg ClawHubConfig) *ClawHubRegistry {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://clawhub.ai"
	}
	searchPath := cfg.SearchPath
	if searchPath == "" {
		searchPath = "/api/v1/search"
	}
	skillsPath := cfg.SkillsPath
	if skillsPath == "" {
		skillsPath = "/api/v1/skills"
	}
	downloadPath := cfg.DownloadPath
	if downloadPath == "" {
		downloadPath = "/api/v1/download"
	}

	timeout := defaultClawHubTimeout
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout) * time.Second
	}

	maxZip := defaultMaxZipSize
	if cfg.MaxZipSize > 0 {
		maxZip = cfg.MaxZipSize
	}

	maxResp := defaultMaxResponseSize
	if cfg.MaxResponseSize > 0 {
		maxResp = cfg.MaxResponseSize
	}

	return &ClawHubRegistry{
		baseURL:         baseURL,
		authToken:       cfg.AuthToken,
		searchPath:      searchPath,
		skillsPath:      skillsPath,
		downloadPath:    downloadPath,
		maxZipSize:      maxZip,
		maxResponseSize: maxResp,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        5,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

func (c *ClawHubRegistry) Name() string {
	return "clawhub"
}

func (c *ClawHubRegistry) ResolveInstallDirName(target string) (string, error) {
	_, slug, err := utils.ParseOwnerSkillRef(target)
	if err != nil {
		return "", err
	}
	return slug, nil
}

// NormalizeInstallTarget canonicalizes owner-qualified targets to "@owner/slug".
func (c *ClawHubRegistry) NormalizeInstallTarget(target string) string {
	owner, slug, err := utils.ParseOwnerSkillRef(target)
	if err != nil {
		return target
	}
	return qualifiedSlug(owner, slug)
}

func qualifiedSlug(owner, slug string) string {
	if owner == "" {
		return slug
	}
	return "@" + owner + "/" + slug
}

func (c *ClawHubRegistry) SkillURL(slug, _ string) string {
	owner, bare, err := utils.ParseOwnerSkillRef(slug)
	if err != nil {
		return ""
	}
	if owner != "" {
		return c.baseURL + "/" + url.PathEscape(owner) + "/skills/" + url.PathEscape(bare)
	}
	return c.baseURL + "/skills/" + url.PathEscape(bare)
}

// ambiguousSlugError turns ClawHub's 409 AMBIGUOUS_SKILL_SLUG response into a
// message telling the user to pick an owner. Returns nil if err is not one.
func ambiguousSlugError(slug string, err error) error {
	if err == nil || !strings.Contains(err.Error(), "AMBIGUOUS_SKILL_SLUG") {
		return nil
	}
	hint := "use the owner-qualified form @owner/slug"
	if i := strings.Index(err.Error(), "{"); i >= 0 {
		var amb struct {
			Matches []struct {
				Ref string `json:"ref"`
			} `json:"matches"`
		}
		if json.Unmarshal([]byte(err.Error()[i:]), &amb) == nil && len(amb.Matches) > 0 {
			refs := make([]string, 0, len(amb.Matches))
			for _, m := range amb.Matches {
				refs = append(refs, m.Ref)
			}
			hint += " (candidates: " + strings.Join(refs, ", ") + ")"
		}
	}
	return fmt.Errorf("skill slug %q is ambiguous on ClawHub: %s", slug, hint)
}

func (c ClawHubConfig) IsEnabled() bool {
	return c.Enabled
}

func (c ClawHubConfig) BuildRegistry() SkillRegistry {
	return NewClawHubRegistry(c)
}

// --- Search ---

type clawhubSearchResponse struct {
	Results []clawhubSearchResult `json:"results"`
}

type clawhubSearchResult struct {
	Score       float64 `json:"score"`
	Slug        *string `json:"slug"`
	OwnerHandle *string `json:"ownerHandle"`
	DisplayName *string `json:"displayName"`
	Summary     *string `json:"summary"`
	Version     *string `json:"version"`
}

func (c *ClawHubRegistry) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	u, err := url.Parse(c.baseURL + c.searchPath)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	q := u.Query()
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}

	var resp clawhubSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}

	results := make([]SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		slug := utils.DerefStr(r.Slug, "")
		if slug == "" {
			continue
		}

		summary := utils.DerefStr(r.Summary, "")
		if summary == "" {
			continue
		}

		displayName := utils.DerefStr(r.DisplayName, "")
		if displayName == "" {
			displayName = slug
		}

		results = append(results, SearchResult{
			Score:        r.Score,
			Slug:         qualifiedSlug(utils.DerefStr(r.OwnerHandle, ""), slug),
			DisplayName:  displayName,
			Summary:      summary,
			Version:      utils.DerefStr(r.Version, ""),
			RegistryName: c.Name(),
		})
	}

	return results, nil
}

// --- GetSkillMeta ---

// clawhubSkillResponse accepts both the current nested shape ({"skill":{...}})
// and the legacy flat shape (slug/displayName/summary at the top level).
type clawhubSkillResponse struct {
	Slug          string                 `json:"slug"`
	DisplayName   string                 `json:"displayName"`
	Summary       string                 `json:"summary"`
	Skill         *clawhubSkillInfo      `json:"skill"`
	Owner         *clawhubOwnerInfo      `json:"owner"`
	LatestVersion *clawhubVersionInfo    `json:"latestVersion"`
	Moderation    *clawhubModerationInfo `json:"moderation"`
}

type clawhubSkillInfo struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName"`
	Summary     string `json:"summary"`
}

type clawhubOwnerInfo struct {
	Handle string `json:"handle"`
}

type clawhubVersionInfo struct {
	Version string `json:"version"`
}

type clawhubModerationInfo struct {
	IsMalwareBlocked bool `json:"isMalwareBlocked"`
	IsSuspicious     bool `json:"isSuspicious"`
}

func (c *ClawHubRegistry) GetSkillMeta(ctx context.Context, ref string) (*SkillMeta, error) {
	owner, slug, err := utils.ParseOwnerSkillRef(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid slug %q: error: %s", ref, err.Error())
	}

	u := c.baseURL + c.skillsPath + "/" + url.PathEscape(slug)
	if owner != "" {
		u += "?" + url.Values{"ownerHandle": {owner}}.Encode()
	}

	body, err := c.doGet(ctx, u)
	if err != nil {
		if ambErr := ambiguousSlugError(slug, err); ambErr != nil {
			return nil, ambErr
		}
		return nil, fmt.Errorf("skill metadata request failed: %w", err)
	}

	var resp clawhubSkillResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse skill metadata: %w", err)
	}

	if resp.Skill != nil {
		resp.Slug, resp.DisplayName, resp.Summary = resp.Skill.Slug, resp.Skill.DisplayName, resp.Skill.Summary
	}
	metaOwner := owner
	if resp.Owner != nil && resp.Owner.Handle != "" {
		metaOwner = resp.Owner.Handle
	}
	meta := &SkillMeta{
		Slug:         qualifiedSlug(metaOwner, resp.Slug),
		DisplayName:  resp.DisplayName,
		Summary:      resp.Summary,
		RegistryName: c.Name(),
	}

	if resp.LatestVersion != nil {
		meta.LatestVersion = resp.LatestVersion.Version
	}
	if resp.Moderation != nil {
		meta.IsMalwareBlocked = resp.Moderation.IsMalwareBlocked
		meta.IsSuspicious = resp.Moderation.IsSuspicious
	}

	return meta, nil
}

// --- DownloadAndInstall ---

// DownloadAndInstall fetches metadata (with fallback), resolves version,
// downloads the skill ZIP, and extracts it to targetDir.
// Returns an InstallResult for the caller to use for moderation decisions.
func (c *ClawHubRegistry) DownloadAndInstall(
	ctx context.Context,
	ref, version, targetDir string,
) (*InstallResult, error) {
	owner, slug, err := utils.ParseOwnerSkillRef(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid slug %q: error: %s", ref, err.Error())
	}

	// Step 1: Fetch metadata (with fallback).
	result := &InstallResult{}
	meta, err := c.GetSkillMeta(ctx, ref)
	if err != nil {
		if strings.Contains(err.Error(), "is ambiguous on ClawHub") {
			return nil, err
		}
		// Fallback: proceed without metadata.
		meta = nil
	}

	if meta != nil {
		result.IsMalwareBlocked = meta.IsMalwareBlocked
		result.IsSuspicious = meta.IsSuspicious
		result.Summary = meta.Summary
	}

	// Step 2: Resolve version.
	installVersion := version
	if installVersion == "" && meta != nil {
		installVersion = meta.LatestVersion
	}
	if installVersion == "" {
		installVersion = "latest"
	}
	result.Version = installVersion

	// Step 3: Download ZIP to temp file (streams in ~32KB chunks).
	u, err := url.Parse(c.baseURL + c.downloadPath)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	q := u.Query()
	q.Set("slug", slug)
	if owner != "" {
		q.Set("ownerHandle", owner)
	}
	if installVersion != "latest" {
		q.Set("version", installVersion)
	}
	u.RawQuery = q.Encode()

	tmpPath, err := c.downloadToTempFileWithRetry(ctx, u.String())
	if err != nil {
		if ambErr := ambiguousSlugError(slug, err); ambErr != nil {
			return nil, ambErr
		}
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer os.Remove(tmpPath)

	// Step 4: Extract from file on disk.
	if err := utils.ExtractZipFile(tmpPath, targetDir); err != nil {
		return nil, err
	}

	return result, nil
}

// --- HTTP helper ---

func (c *ClawHubRegistry) doGet(ctx context.Context, urlStr string) ([]byte, error) {
	req, err := c.newGetRequest(ctx, urlStr, "application/json")
	if err != nil {
		return nil, err
	}

	resp, err := utils.DoRequestWithRetry(c.client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Limit response body read to prevent memory issues.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxResponseSize)))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

func (c *ClawHubRegistry) newGetRequest(ctx context.Context, urlStr, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	return req, nil
}

func (c *ClawHubRegistry) downloadToTempFileWithRetry(ctx context.Context, urlStr string) (string, error) {
	req, err := c.newGetRequest(ctx, urlStr, "application/zip")
	if err != nil {
		return "", err
	}

	resp, err := utils.DoRequestWithRetry(c.client, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, errBody)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(errBody[:n]))
	}

	tmpFile, err := os.CreateTemp("", "picoclaw-dl-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	src := io.LimitReader(resp.Body, int64(c.maxZipSize)+1)
	written, err := io.Copy(tmpFile, src)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("download write failed: %w", err)
	}

	if written > int64(c.maxZipSize) {
		cleanup()
		return "", fmt.Errorf("download too large: %d bytes (max %d)", written, c.maxZipSize)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	return tmpPath, nil
}

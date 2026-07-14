package indexing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

const (
	manifestSchemaVersion = 1
	maxManifestBytes      = 512 << 10
	maxArtifactBytes      = 64 << 20
	maxRedirects          = 3
)

const (
	officialManifestURL      = "https://github.com/tae2089/thread-keep/releases/latest/download/thread-keep-indexers-manifest-v1.json"
	officialManifestPrefix   = "https://github.com/tae2089/thread-keep/releases/"
	officialArtifactPrefix   = "https://github.com/tae2089/thread-keep/releases/download/"
	officialObjectsHost      = "objects.githubusercontent.com"
	officialReleaseAssetHost = "release-assets.githubusercontent.com"
)

var officialManifestPublicKeyBase64 string

type Installer struct {
	Client                *http.Client
	ManifestURL           string
	PublicKey             ed25519.PublicKey
	GOOS                  string
	GOARCH                string
	UserConfigDir         func() (string, error)
	TrustedManifestPrefix string
	TrustedArtifactPrefix string
	AllowedRedirectHosts  map[string]bool
	AllowHTTP             bool
	ExpectedVersion       string
}

type manifestEnvelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type manifest struct {
	SchemaVersion int            `json:"schema_version"`
	Packs         []manifestPack `json:"packs"`
}

type manifestPack struct {
	ID              string          `json:"id"`
	Version         string          `json:"version"`
	ProtocolVersion int             `json:"protocol_version"`
	Assets          []manifestAsset `json:"assets"`
}

type manifestAsset struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func InstallDetected(ctx context.Context, root string) ([]domain.IndexerStatus, error) {
	statuses, err := List(ctx, root)
	if err != nil {
		return nil, err
	}
	var languages []Language
	for _, status := range statuses {
		if status.State == domain.IndexerMissing && status.Detected {
			languages = append(languages, Language(status.Language))
		}
	}
	if len(languages) == 0 {
		return statuses, nil
	}
	installer, err := newOfficialInstaller()
	if err != nil {
		return nil, err
	}
	if err := installer.Install(ctx, languages); err != nil {
		return nil, err
	}
	return List(ctx, root)
}

func SyncDetected(ctx context.Context, root, version string) ([]domain.IndexerStatus, error) {
	if version != "" && !validReleaseVersion(version) {
		return nil, domain.NewError(domain.CodeValidation, errors.New("indexer version must be stable SemVer X.Y.Z"))
	}
	candidates, err := DetectContext(ctx, root)
	if err != nil {
		return nil, err
	}
	var languages []Language
	for _, candidate := range candidates {
		if isExternalPackLanguage(candidate.Language) {
			languages = append(languages, candidate.Language)
		}
	}
	if len(languages) == 0 {
		return List(ctx, root)
	}
	installer, err := newOfficialInstallerForVersion(version)
	if err != nil {
		return nil, err
	}
	if err := installer.Sync(ctx, languages); err != nil {
		return nil, err
	}
	return List(ctx, root)
}

func newOfficialInstaller() (Installer, error) {
	return newOfficialInstallerForVersion("")
}

func newOfficialInstallerForVersion(version string) (Installer, error) {
	manifestURL, err := officialManifestURLForVersion(version)
	if err != nil {
		return Installer{}, err
	}
	publicKey, err := decodePublicKey(officialManifestPublicKeyBase64)
	if err != nil {
		return Installer{}, err
	}
	return Installer{
		Client:                &http.Client{Timeout: 30 * time.Second},
		ManifestURL:           manifestURL,
		PublicKey:             publicKey,
		GOOS:                  runtime.GOOS,
		GOARCH:                runtime.GOARCH,
		UserConfigDir:         os.UserConfigDir,
		TrustedManifestPrefix: officialManifestPrefix,
		TrustedArtifactPrefix: officialArtifactPrefix,
		AllowedRedirectHosts: map[string]bool{
			"github.com":             true,
			officialObjectsHost:      true,
			officialReleaseAssetHost: true,
		},
		ExpectedVersion: version,
	}, nil
}

func officialManifestURLForVersion(version string) (string, error) {
	if version == "" {
		return officialManifestURL, nil
	}
	if !validReleaseVersion(version) {
		return "", domain.NewError(domain.CodeValidation, errors.New("indexer version must be stable SemVer X.Y.Z"))
	}
	return officialArtifactPrefix + "v" + version + "/thread-keep-indexers-manifest-v1.json", nil
}

func decodePublicKey(value string) (ed25519.PublicKey, error) {
	if strings.TrimSpace(value) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("official manifest verification key is not configured"))
	}
	contents, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(contents) != ed25519.PublicKeySize {
		return nil, domain.NewError(domain.CodeValidation, errors.New("official manifest verification key is invalid"))
	}
	return ed25519.PublicKey(contents), nil
}

func (i Installer) Install(ctx context.Context, languages []Language) error {
	return i.install(ctx, languages, false)
}

func (i Installer) Sync(ctx context.Context, languages []Language) error {
	return i.install(ctx, languages, true)
}

func (i Installer) install(ctx context.Context, languages []Language, replace bool) error {
	if err := i.validate(); err != nil {
		return err
	}
	requested, err := normalizeLanguages(languages)
	if err != nil {
		return err
	}
	if len(requested) == 0 {
		return nil
	}
	if !replace {
		configDir, err := i.UserConfigDir()
		if err != nil {
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("locate user configuration directory: %w", err))
		}
		for _, language := range requested {
			if _, found, err := resolveInstalledPack(configDir, language); err != nil {
				return err
			} else if found {
				return domain.NewError(domain.CodeBusy, fmt.Errorf("an executable %s pack already exists", language))
			}
		}
	}
	contents, err := i.fetchBytes(ctx, i.ManifestURL, maxManifestBytes, i.validateManifestURL)
	if err != nil {
		return err
	}
	manifest, err := i.verifyManifest(contents)
	if err != nil {
		return err
	}
	for _, language := range requested {
		pack, asset, err := i.selectAsset(manifest, language)
		if err != nil {
			return err
		}
		if err := i.installAsset(ctx, language, pack, asset); err != nil {
			return err
		}
	}
	return nil
}

func (i Installer) validate() error {
	if i.Client == nil || i.UserConfigDir == nil || strings.TrimSpace(i.ManifestURL) == "" || strings.TrimSpace(i.GOOS) == "" || strings.TrimSpace(i.GOARCH) == "" {
		return domain.NewError(domain.CodeValidation, errors.New("installer is not configured"))
	}
	if len(i.PublicKey) != ed25519.PublicKeySize {
		return domain.NewError(domain.CodeValidation, errors.New("official manifest verification key is invalid"))
	}
	if i.ExpectedVersion != "" && !validReleaseVersion(i.ExpectedVersion) {
		return domain.NewError(domain.CodeValidation, errors.New("expected indexer version must be stable SemVer X.Y.Z"))
	}
	if _, err := i.parseURL(i.ManifestURL); err != nil {
		return err
	}
	return nil
}

func normalizeLanguages(languages []Language) ([]Language, error) {
	seen := make(map[Language]struct{}, len(languages))
	for _, language := range languages {
		if !isExternalPackLanguage(language) {
			return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("no installable official pack for %q", language))
		}
		seen[language] = struct{}{}
	}
	ordered := make([]Language, 0, len(seen))
	for language := range seen {
		ordered = append(ordered, language)
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	return ordered, nil
}

func (i Installer) verifyManifest(contents []byte) (manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var envelope manifestEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return manifest{}, domain.NewError(domain.CodeValidation, fmt.Errorf("decode signed pack manifest: %w", err))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return manifest{}, domain.NewError(domain.CodeValidation, errors.New("signed pack manifest contains more than one JSON value"))
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return manifest{}, domain.NewError(domain.CodeValidation, errors.New("signed pack manifest payload is invalid"))
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(i.PublicKey, payload, signature) {
		return manifest{}, domain.NewError(domain.CodeValidation, errors.New("signed pack manifest signature is invalid"))
	}
	decoder = json.NewDecoder(bytes.NewReader(payload))
	var value manifest
	if err := decoder.Decode(&value); err != nil {
		return manifest{}, domain.NewError(domain.CodeValidation, fmt.Errorf("decode verified pack manifest: %w", err))
	}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return manifest{}, domain.NewError(domain.CodeValidation, errors.New("verified pack manifest contains more than one JSON value"))
	}
	if err := i.validateManifest(value); err != nil {
		return manifest{}, err
	}
	return value, nil
}

func (i Installer) validateManifest(value manifest) error {
	if value.SchemaVersion != manifestSchemaVersion || len(value.Packs) == 0 {
		return domain.NewError(domain.CodeValidation, errors.New("verified pack manifest schema is unsupported"))
	}
	packs := make(map[string]struct{}, len(value.Packs))
	for _, pack := range value.Packs {
		if !isExternalPackID(pack.ID) || !validReleaseVersion(pack.Version) || pack.ProtocolVersion != protocolVersion || len(pack.Assets) == 0 {
			return domain.NewError(domain.CodeValidation, errors.New("verified pack manifest contains an invalid pack"))
		}
		if i.ExpectedVersion != "" && pack.Version != i.ExpectedVersion {
			return domain.NewError(domain.CodeValidation, errors.New("verified pack manifest version does not match the requested version"))
		}
		if _, found := packs[pack.ID]; found {
			return domain.NewError(domain.CodeValidation, errors.New("verified pack manifest contains duplicate pack IDs"))
		}
		packs[pack.ID] = struct{}{}
		platforms := make(map[string]struct{}, len(pack.Assets))
		for _, asset := range pack.Assets {
			if strings.TrimSpace(asset.GOOS) == "" || strings.TrimSpace(asset.GOARCH) == "" || asset.Size <= 0 || asset.Size > maxArtifactBytes || !validSHA256(asset.SHA256) {
				return domain.NewError(domain.CodeValidation, errors.New("verified pack manifest contains an invalid asset"))
			}
			if err := i.validateArtifactURL(asset.URL); err != nil {
				return err
			}
			platform := asset.GOOS + "/" + asset.GOARCH
			if _, found := platforms[platform]; found {
				return domain.NewError(domain.CodeValidation, errors.New("verified pack manifest contains duplicate platform assets"))
			}
			platforms[platform] = struct{}{}
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (i Installer) selectAsset(value manifest, language Language) (manifestPack, manifestAsset, error) {
	wantID := packID(language)
	for _, pack := range value.Packs {
		if pack.ID != wantID {
			continue
		}
		for _, asset := range pack.Assets {
			if asset.GOOS == i.GOOS && asset.GOARCH == i.GOARCH {
				return pack, asset, nil
			}
		}
	}
	return manifestPack{}, manifestAsset{}, domain.NewError(domain.CodeValidation, fmt.Errorf("no official %s pack for %s/%s", language, i.GOOS, i.GOARCH))
}

func (i Installer) installAsset(ctx context.Context, language Language, pack manifestPack, asset manifestAsset) error {
	configDir, err := i.UserConfigDir()
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("locate user configuration directory: %w", err))
	}
	directory := packObjectDirectory(configDir, language)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack directory: %w", err))
	}
	target := packObjectPathForGOOS(configDir, language, asset.SHA256, i.GOOS)
	temporary, err := os.CreateTemp(directory, "."+packID(language)+"-*")
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack temp file: %w", err))
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	response, err := i.open(ctx, asset.URL, i.validateArtifactURL)
	if err != nil {
		_ = temporary.Close()
		return err
	}
	defer response.Body.Close()
	if response.ContentLength >= 0 && response.ContentLength != asset.Size {
		_ = temporary.Close()
		return domain.NewError(domain.CodeValidation, errors.New("pack artifact size does not match the signed manifest"))
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, asset.Size+1))
	if err != nil {
		_ = temporary.Close()
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("download pack artifact: %w", err))
	}
	if written != asset.Size {
		_ = temporary.Close()
		return domain.NewError(domain.CodeValidation, errors.New("pack artifact size does not match the signed manifest"))
	}
	if hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		_ = temporary.Close()
		return domain.NewError(domain.CodeValidation, errors.New("pack artifact digest does not match the signed manifest"))
	}
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("mark pack executable: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("sync pack artifact: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("close pack artifact: %w", err))
	}
	if err := publishImmutablePack(temporaryName, target, asset); err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		return domain.NewError(domain.CodeLocalStorage, err)
	}
	return i.activatePack(configDir, language, pack, asset)
}

func (i Installer) activatePack(configDir string, language Language, pack manifestPack, asset manifestAsset) error {
	directory := packDirectory(configDir)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack directory: %w", err))
	}
	activation := packActivation{SchemaVersion: packActivationSchemaVersion, PackID: pack.ID, Version: pack.Version, ProtocolVersion: pack.ProtocolVersion, Size: asset.Size, SHA256: asset.SHA256}
	temporary, err := os.CreateTemp(directory, "."+packID(language)+"-activation-*")
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack activation temp file: %w", err))
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(activation); err != nil {
		_ = temporary.Close()
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("encode pack activation: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("sync pack activation: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("close pack activation: %w", err))
	}
	if err := os.Rename(temporaryName, packActivationPath(configDir, language)); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("activate pack: %w", err))
	}
	if err := syncDirectory(directory); err != nil {
		return domain.NewError(domain.CodeLocalStorage, err)
	}
	return nil
}

func publishImmutablePack(temporaryName, target string, asset manifestAsset) error {
	if err := os.Link(temporaryName, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("publish pack artifact: %w", err))
		}
		if verifyErr := verifyPackArtifact(target, asset.Size, asset.SHA256); verifyErr != nil {
			return verifyErr
		}
	}
	if err := os.Remove(temporaryName); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("remove published pack temp file: %w", err))
	}
	return nil
}

func (i Installer) fetchBytes(ctx context.Context, rawURL string, limit int64, validate func(string) error) ([]byte, error) {
	response, err := i.open(ctx, rawURL, validate)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.ContentLength > limit {
		return nil, domain.NewError(domain.CodeLocalStorage, errors.New("pack response exceeds configured size limit"))
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read pack response: %w", err))
	}
	if int64(len(contents)) > limit {
		return nil, domain.NewError(domain.CodeLocalStorage, errors.New("pack response exceeds configured size limit"))
	}
	return contents, nil
}

func (i Installer) open(ctx context.Context, rawURL string, validateInitial func(string) error) (*http.Response, error) {
	if err := validateInitial(rawURL); err != nil {
		return nil, err
	}
	client := *i.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	current := rawURL
	for redirects := 0; ; redirects++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("build pack request: %w", err))
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("request pack resource: %w", err))
		}
		if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
			if redirects == maxRedirects {
				response.Body.Close()
				return nil, domain.NewError(domain.CodeValidation, errors.New("pack resource exceeds redirect limit"))
			}
			next, err := response.Location()
			response.Body.Close()
			if err != nil {
				return nil, domain.NewError(domain.CodeValidation, errors.New("pack redirect location is invalid"))
			}
			currentURL, err := url.Parse(current)
			if err != nil {
				return nil, domain.NewError(domain.CodeValidation, errors.New("pack resource URL is invalid"))
			}
			next = currentURL.ResolveReference(next)
			if err := i.validateRedirectURL(next); err != nil {
				return nil, err
			}
			current = next.String()
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("pack resource returned HTTP %d", response.StatusCode))
		}
		return response, nil
	}
}

func (i Installer) validateManifestURL(value string) error {
	if err := i.validateURL(value); err != nil {
		return err
	}
	if i.TrustedManifestPrefix != "" && !hasURLPrefix(value, i.TrustedManifestPrefix) {
		return domain.NewError(domain.CodeValidation, errors.New("manifest URL is outside the official release origin"))
	}
	return nil
}

func (i Installer) validateArtifactURL(value string) error {
	if err := i.validateURL(value); err != nil {
		return err
	}
	if i.TrustedArtifactPrefix != "" && !hasURLPrefix(value, i.TrustedArtifactPrefix) {
		return domain.NewError(domain.CodeValidation, errors.New("pack artifact URL is outside the official release origin"))
	}
	return nil
}

func (i Installer) validateURL(value string) error {
	parsed, err := i.parseURL(value)
	if err != nil {
		return err
	}
	if !i.AllowHTTP && parsed.Scheme != "https" {
		return domain.NewError(domain.CodeValidation, errors.New("pack resource URL must use HTTPS"))
	}
	if !i.AllowHTTP && parsed.Port() != "" && parsed.Port() != "443" {
		return domain.NewError(domain.CodeValidation, errors.New("pack resource URL must use the default HTTPS port"))
	}
	return nil
}

func (i Installer) parseURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("pack resource URL is invalid"))
	}
	return parsed, nil
}

func (i Installer) validateRedirectURL(value *url.URL) error {
	if value == nil || value.User != nil || value.Host == "" || value.Scheme == "" {
		return domain.NewError(domain.CodeValidation, errors.New("pack redirect URL is invalid"))
	}
	if !i.AllowHTTP && value.Scheme != "https" {
		return domain.NewError(domain.CodeValidation, errors.New("pack redirect URL must use HTTPS"))
	}
	if !i.AllowHTTP && value.Port() != "" && value.Port() != "443" {
		return domain.NewError(domain.CodeValidation, errors.New("pack redirect URL must use the default HTTPS port"))
	}
	if len(i.AllowedRedirectHosts) == 0 {
		if hasURLPrefix(value.String(), i.TrustedManifestPrefix) || hasURLPrefix(value.String(), i.TrustedArtifactPrefix) {
			return nil
		}
		return domain.NewError(domain.CodeValidation, errors.New("pack redirect leaves the trusted release origin"))
	}
	if !i.AllowedRedirectHosts[value.Hostname()] {
		return domain.NewError(domain.CodeValidation, errors.New("pack redirect leaves the trusted release origin"))
	}
	return nil
}

func hasURLPrefix(value, prefix string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	trusted, err := url.Parse(prefix)
	if err != nil || trusted.Scheme != parsed.Scheme || trusted.Host != parsed.Host {
		return false
	}
	return strings.HasPrefix(parsed.EscapedPath(), trusted.EscapedPath())
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open pack directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync pack directory: %w", err)
	}
	return nil
}

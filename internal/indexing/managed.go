package indexing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

const (
	packActivationSchemaVersion = 1
	maxPackActivationBytes      = 16 << 10
)

type packActivation struct {
	SchemaVersion   int    `json:"schema_version"`
	PackID          string `json:"pack_id"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
}

type installedPack struct {
	Path       string
	Descriptor Descriptor
	SHA256     string
}

func loadPackActivation(configDir string, language Language) (packActivation, error) {
	path := packActivationPath(configDir, language)
	info, err := os.Lstat(path)
	if err != nil {
		return packActivation{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxPackActivationBytes {
		return packActivation{}, domain.NewError(domain.CodeValidation, fmt.Errorf("%s pack activation is not a bounded regular file", language))
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return packActivation{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var activation packActivation
	if err := decoder.Decode(&activation); err != nil {
		return packActivation{}, domain.NewError(domain.CodeValidation, fmt.Errorf("decode %s pack activation: %w", language, err))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return packActivation{}, domain.NewError(domain.CodeValidation, fmt.Errorf("%s pack activation contains more than one JSON value", language))
	}
	if err := validatePackActivation(language, activation); err != nil {
		return packActivation{}, err
	}
	return activation, nil
}

func resolveManagedPack(configDir string, language Language) (installedPack, bool, error) {
	activation, err := loadPackActivation(configDir, language)
	if errors.Is(err, os.ErrNotExist) {
		return installedPack{}, false, nil
	}
	if err != nil {
		return installedPack{}, false, err
	}
	path := packObjectPath(configDir, language, activation.SHA256)
	if err := verifyPackArtifact(path, activation.Size, activation.SHA256); err != nil {
		return installedPack{}, false, err
	}
	return installedPack{
		Path: path,
		Descriptor: Descriptor{
			ID:      activation.PackID,
			Version: activation.Version,
		},
		SHA256: activation.SHA256,
	}, true, nil
}

func resolveInstalledPack(configDir string, language Language) (installedPack, bool, error) {
	managed, found, err := resolveManagedPack(configDir, language)
	if err != nil || found {
		return managed, found, err
	}
	path := filepath.Join(packDirectory(configDir), packExecutableName(packID(language)))
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !usablePackFile(info)) {
		return installedPack{}, false, nil
	}
	if err != nil {
		return installedPack{}, false, fmt.Errorf("inspect %s pack: %w", language, err)
	}
	return installedPack{Path: path, Descriptor: Descriptor{ID: packID(language)}}, true, nil
}

func resolveAvailablePack(configDir, bundledDirectory, bundledVersion string, language Language) (installedPack, bool, error) {
	installed, found, err := resolveInstalledPack(configDir, language)
	if err != nil || found {
		return installed, found, err
	}
	return resolveBundledPack(bundledDirectory, bundledVersion, language)
}

func resolveBundledPack(directory, version string, language Language) (installedPack, bool, error) {
	directory = strings.TrimSpace(directory)
	version = strings.TrimSpace(version)
	if directory == "" && version == "" {
		return installedPack{}, false, nil
	}
	if directory == "" || !filepath.IsAbs(directory) || !validReleaseVersion(version) {
		return installedPack{}, false, domain.NewError(domain.CodeValidation, errors.New("bundled pack directory and version are invalid"))
	}
	path := filepath.Join(directory, packExecutableName(packID(language)))
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && !usablePackFile(info) {
		return installedPack{}, false, nil
	}
	if err != nil {
		return installedPack{}, false, fmt.Errorf("inspect bundled %s pack: %w", language, err)
	}
	return installedPack{Path: path, Descriptor: Descriptor{ID: packID(language), Version: version}}, true, nil
}

func validatePackActivation(language Language, activation packActivation) error {
	if activation.SchemaVersion != packActivationSchemaVersion || activation.PackID != packID(language) || !validReleaseVersion(activation.Version) || activation.ProtocolVersion != protocolVersion || activation.Size <= 0 || activation.Size > maxArtifactBytes || !validSHA256(activation.SHA256) {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("%s pack activation is invalid", language))
	}
	return nil
}

func verifyPackArtifact(path string, size int64, digest string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("inspect managed pack artifact: %w", err))
	}
	if !usablePackFile(info) || info.Size() != size {
		return domain.NewError(domain.CodeValidation, errors.New("managed pack artifact does not match its activation metadata"))
	}
	file, err := os.Open(path)
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("open managed pack artifact: %w", err))
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, size+1))
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("hash managed pack artifact: %w", err))
	}
	if written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return domain.NewError(domain.CodeValidation, errors.New("managed pack artifact digest does not match its activation metadata"))
	}
	return nil
}

func validReleaseVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func packDirectory(configDir string) string {
	return filepath.Join(configDir, "thread-keep", "packs")
}

func packActivationPath(configDir string, language Language) string {
	return filepath.Join(packDirectory(configDir), packID(language)+".json")
}

func packObjectDirectory(configDir string, language Language) string {
	return filepath.Join(packDirectory(configDir), "objects", packID(language))
}

func packObjectPath(configDir string, language Language, digest string) string {
	return packObjectPathForGOOS(configDir, language, digest, runtime.GOOS)
}

func packObjectPathForGOOS(configDir string, language Language, digest, goos string) string {
	return filepath.Join(packObjectDirectory(configDir, language), packExecutableNameForGOOS(digest, goos))
}

func packExecutableName(name string) string {
	return packExecutableNameForGOOS(name, runtime.GOOS)
}

func packExecutableNameForGOOS(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

func usablePackFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0)
}

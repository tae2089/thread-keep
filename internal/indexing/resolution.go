package indexing

import (
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

type installedPack struct {
	Path       string
	Descriptor Descriptor
}

type pypiPackEntry struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

func resolveAvailablePack(configDir, pypiMapping, packageVersion string, language Language) (installedPack, bool, error) {
	pypi, found, err := resolvePyPIPack(pypiMapping, packageVersion, language)
	if err != nil || found {
		return pypi, found, err
	}
	return resolveInstalledPack(configDir, language)
}

func resolvePyPIPack(value, packageVersion string, language Language) (installedPack, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return installedPack{}, false, nil
	}
	packageVersion = strings.TrimSpace(packageVersion)
	if !validReleaseVersion(packageVersion) {
		return installedPack{}, false, domain.NewError(domain.CodeValidation, errors.New("PyPI package version is invalid"))
	}
	entries, err := decodePyPIPacks(value)
	if err != nil {
		return installedPack{}, false, domain.NewError(domain.CodeValidation, fmt.Errorf("PyPI pack mapping is invalid: %w", err))
	}
	for name, entry := range entries {
		entryLanguage, known := externalPackLanguage(name)
		if !known || !filepath.IsAbs(entry.Path) || filepath.Base(entry.Path) != packExecutableName(packID(entryLanguage)) || entry.Version != packageVersion {
			return installedPack{}, false, domain.NewError(domain.CodeValidation, fmt.Errorf("PyPI pack entry %q is invalid", name))
		}
	}
	entry, found := entries[string(language)]
	if !found {
		return installedPack{}, false, nil
	}
	info, err := os.Stat(entry.Path)
	if errors.Is(err, os.ErrNotExist) || err == nil && !usablePackFile(info) {
		return installedPack{}, false, nil
	}
	if err != nil {
		return installedPack{}, false, fmt.Errorf("inspect PyPI %s pack: %w", language, err)
	}
	return installedPack{Path: entry.Path, Descriptor: Descriptor{ID: packID(language), Version: entry.Version}}, true, nil
}

func decodePyPIPacks(value string) (map[string]pypiPackEntry, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var entries map[string]pypiPackEntry
	if err := decoder.Decode(&entries); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected one JSON object")
	}
	if entries == nil {
		return nil, errors.New("expected one JSON object")
	}
	return entries, nil
}

func externalPackLanguage(value string) (Language, bool) {
	for _, language := range externalPackLanguages {
		if string(language) == value {
			return language, true
		}
	}
	return "", false
}

func resolveInstalledPack(configDir string, language Language) (installedPack, bool, error) {
	path := filepath.Join(packDirectory(configDir), packExecutableName(packID(language)))
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && !usablePackFile(info) {
		return installedPack{}, false, nil
	}
	if err != nil {
		return installedPack{}, false, fmt.Errorf("inspect %s pack: %w", language, err)
	}
	return installedPack{Path: path, Descriptor: Descriptor{ID: packID(language)}}, true, nil
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

func packExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func usablePackFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0)
}

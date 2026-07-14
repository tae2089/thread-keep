package indexing

import (
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

func validRelativePath(value string) bool {
	return value != "" && !filepath.IsAbs(value) && !strings.Contains(value, "\\") && pathpkg.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func validKind(kind domain.EntityKind) bool {
	switch kind {
	case domain.EntityFunction, domain.EntityMethod, domain.EntityType, domain.EntityClass, domain.EntityInterface, domain.EntityEnum:
		return true
	default:
		return false
	}
}

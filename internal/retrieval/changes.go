package retrieval

import "github.com/tae2089/thread-keep/internal/domain"

func ClassifyChanges(base, current []domain.Entity) []domain.EntityChange {
	return domain.ClassifyEntityChanges(base, current)
}

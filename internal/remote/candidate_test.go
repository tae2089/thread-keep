package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestHTTPCandidatePublicationReadsMetadataAndPublishesDigest(t *testing.T) {
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	metadata := CandidatePublicationMetadata{Change: change, BaseSourceSHA: strings.Repeat("1", 40), HeadSourceSHA: strings.Repeat("2", 40), BaseContextCommitID: strings.Repeat("3", 64)}
	delta, err := domain.NormalizeCandidateContextDelta(domain.CandidateContextDelta{SchemaVersion: 2, Change: change, BaseSourceSHA: metadata.BaseSourceSHA, HeadSourceSHA: metadata.HeadSourceSHA, BaseContextCommitID: metadata.BaseContextCommitID})
	if err != nil {
		t.Fatalf("NormalizeCandidateContextDelta() error = %v", err)
	}
	digest, err := domain.CandidateContextDigest(delta)
	if err != nil {
		t.Fatalf("CandidateContextDigest() error = %v", err)
	}
	var put CandidatePublicationRequest
	putCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/repositories/repo/pull-requests/42/candidate" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(writer).Encode(metadata)
		case http.MethodPut:
			putCount++
			if err := json.NewDecoder(request.Body).Decode(&put); err != nil {
				t.Errorf("decode candidate request: %v", err)
			}
			if putCount == 1 {
				writer.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(writer).Encode(CandidatePublicationResult{Digest: digest, Published: true})
				return
			}
			_ = json.NewEncoder(writer).Encode(CandidatePublicationResult{Digest: digest, Published: false})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	client := NewHTTP(server.URL+"/v1/repositories/repo", "token")

	gotMetadata, err := client.CandidatePublicationMetadata(context.Background(), change)
	if err != nil || gotMetadata != metadata {
		t.Fatalf("CandidatePublicationMetadata() = %+v, %v, want %+v", gotMetadata, err, metadata)
	}
	result, err := client.PublishCandidate(context.Background(), CandidatePublicationRequest{Delta: delta, Digest: digest})
	if err != nil {
		t.Fatalf("PublishCandidate() error = %v", err)
	}
	if !result.Published || result.Digest != digest || put.Digest != digest || put.Delta.Change != change {
		t.Fatalf("PublishCandidate() result=%+v request=%+v", result, put)
	}
	result, err = client.PublishCandidate(context.Background(), CandidatePublicationRequest{Delta: delta, Digest: digest})
	if err != nil || result.Published || result.Digest != digest {
		t.Fatalf("PublishCandidate(existing) = %+v, %v, want no-op", result, err)
	}
}

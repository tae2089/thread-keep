package backend

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/zeebo/blake3"
)

const (
	requestDigestDomain   = "thread-keep-runner-request-v1"
	executionDigestDomain = "thread-keep-runner-execution-v1"
	attemptDigestDomain   = "thread-keep-runner-attempt-v1"
	specDigestDomain      = "thread-keep-runner-spec-v1"
)

func RequestDigest(request planner.SourceRequest) (string, error) {
	if err := planner.ValidateSourceRequest(request); err != nil {
		return "", err
	}
	return digestFields(requestDigestDomain,
		planner.WorkerVersion,
		string(request.Mode),
		request.RepositoryID,
		request.TargetRef,
		request.RepositoryURL,
		strings.ToLower(request.BaseSHA),
		strings.ToLower(request.HeadSHA),
		strings.ToLower(request.FinalSHA),
	), nil
}

func SpecDigest(backend BackendName, fields ...string) string {
	values := append([]string{planner.WorkerVersion, string(backend)}, fields...)
	return digestFields(specDigestDomain, values...)
}

func ExecutionID(jobID, requestDigest string) (string, error) {
	if !validDigest(jobID) || !validDigest(requestDigest) {
		return "", domain.NewError(domain.CodeValidation, errors.New("runner execution identity input is invalid"))
	}
	return digestFields(executionDigestDomain, jobID, requestDigest), nil
}

func AttemptID(executionID string, attempt int) (string, error) {
	if !validDigest(executionID) || attempt < 1 {
		return "", domain.NewError(domain.CodeValidation, errors.New("runner attempt identity input is invalid"))
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(attempt))
	digest := blake3.New()
	writeDigestField(digest, attemptDigestDomain)
	writeDigestField(digest, executionID)
	_, _ = digest.Write(encoded[:])
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func digestFields(domain string, fields ...string) string {
	digest := blake3.New()
	writeDigestField(digest, domain)
	for _, field := range fields {
		writeDigestField(digest, field)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeDigestField(digest hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write([]byte(value))
}

func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

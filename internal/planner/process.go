package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

const (
	defaultProcessTimeout = 2 * time.Minute
	defaultMaxOutputBytes = 16 << 20
	maxWorkerRequestBytes = 1 << 20
)

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

type runnerResponse struct {
	Evidence SourceEvidence   `json:"evidence"`
	Code     domain.ErrorCode `json:"code,omitempty"`
	Message  string           `json:"message,omitempty"`
}

func (e ProcessRunner) IndexSource(ctx context.Context, request SourceRequest) (SourceEvidence, error) {
	if err := validateSourceRequest(request); err != nil {
		return SourceEvidence{}, err
	}
	if e.Path == "" {
		return SourceEvidence{}, domain.NewError(domain.CodeValidation, errors.New("runner path is not configured"))
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultProcessTimeout
	}
	limit := e.MaxOutputBytes
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return SourceEvidence{}, domain.NewError(domain.CodeValidation, errors.New("serialize runner request"))
	}
	workerCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(workerCtx, e.Path, "worker")
	command.Stdin = bytes.NewReader(append(payload, '\n'))
	stdout := boundedBuffer{limit: limit}
	stderr := boundedBuffer{limit: 64 << 10}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if workerCtx.Err() != nil {
			return SourceEvidence{}, domain.NewError(domain.CodeBusy, errors.New("runner timed out or was cancelled"))
		}
		return SourceEvidence{}, domain.NewError(domain.CodeLocalStorage, errors.New("runner failed"))
	}
	if stdout.exceeded {
		return SourceEvidence{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner response exceeds the output limit"))
	}
	var response runnerResponse
	if err := decodeSingleJSON(stdout.Bytes(), &response); err != nil {
		return SourceEvidence{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner returned invalid evidence"))
	}
	if response.Code != "" {
		return SourceEvidence{}, domain.NewError(response.Code, errors.New(response.Message))
	}
	if err := validateSourceEvidence(request, response.Evidence); err != nil {
		return SourceEvidence{}, err
	}
	return response.Evidence, nil
}

func RunWorker(ctx context.Context, input io.Reader, output io.Writer) error {
	contents, err := io.ReadAll(io.LimitReader(input, maxWorkerRequestBytes+1))
	if err != nil || len(contents) > maxWorkerRequestBytes {
		return errors.New("runner request exceeds the input limit")
	}
	var request SourceRequest
	if err := decodeSingleJSON(contents, &request); err != nil {
		return errors.New("runner request is invalid")
	}
	evidence, executeErr := NewNativeRunner(NativeConfig{}).IndexSource(ctx, request)
	response := runnerResponse{Evidence: evidence}
	if executeErr != nil {
		response.Evidence = SourceEvidence{}
		response.Code = domain.CodeOf(executeErr)
		response.Message = safeRunnerMessage(response.Code)
	}
	return json.NewEncoder(output).Encode(response)
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(value), nil
	}
	if len(value) > remaining {
		_, _ = b.Buffer.Write(value[:remaining])
		b.exceeded = true
		return len(value), nil
	}
	return b.Buffer.Write(value)
}

func decodeSingleJSON(contents []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func validateSourceEvidence(request SourceRequest, evidence SourceEvidence) error {
	if evidence.RepositoryID != request.RepositoryID || evidence.TargetRef != request.TargetRef || evidence.Mode != request.Mode || evidence.WorkerVersion != WorkerVersion || evidence.GitTreeDigest == "" || evidence.EntityShapeDigest != domain.DigestSourceEvidence(evidence.Entities) || !evidence.CoverageComplete || len(evidence.Entities) > maxEvidenceEntities {
		return domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner evidence is incomplete or mismatched"))
	}
	if len(evidence.Provenance) == 0 {
		return domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner provenance is incomplete"))
	}
	languages := make(map[string]bool, len(evidence.Provenance))
	for _, item := range evidence.Provenance {
		if item.Language == "" || item.IndexerID == "" || item.IndexerVersion == "" || item.SourceSHA != evidence.SourceSHA || languages[item.Language] {
			return domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner provenance is invalid"))
		}
		languages[item.Language] = true
	}
	for _, entity := range evidence.Entities {
		if entity.SourceSHA != evidence.SourceSHA || !languages[entity.Language] {
			return domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner entities do not match provenance"))
		}
	}
	if request.Mode == SourceFinal && evidence.SourceSHA != strings.ToLower(request.FinalSHA) {
		return domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner final source evidence is stale"))
	}
	if request.Mode == SourcePreview && (evidence.SourceSHA != strings.ToLower(request.HeadSHA) || !validSHA(evidence.PreviewIdentity)) {
		return domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner preview source evidence is stale"))
	}
	return nil
}

func ValidateSourceEvidence(request SourceRequest, evidence SourceEvidence) error {
	return validateSourceEvidence(request, evidence)
}

func safeRunnerMessage(code domain.ErrorCode) string {
	switch code {
	case domain.CodeRemoteConflict:
		return "preview source revisions do not merge cleanly"
	case domain.CodeEntityNotFound:
		return "requested source revision is unavailable"
	case domain.CodeCoverageIncomplete:
		return "source indexing evidence is incomplete"
	case domain.CodeBusy:
		return "runner execution timed out or was cancelled"
	case domain.CodeValidation:
		return "runner request is invalid"
	default:
		return "runner execution failed"
	}
}

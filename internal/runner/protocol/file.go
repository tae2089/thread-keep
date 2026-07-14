package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
)

const (
	maxFileRequestBytes = 1 << 20
	maxCredentialBytes  = 64 << 10
)

type FileExecutionOptions struct {
	RequestPath           string
	CredentialPath        string
	ResultPath            string
	CredentialWaitTimeout time.Duration
}

func ExecuteFiles(ctx context.Context, options FileExecutionOptions, runner planner.SourceRunner) error {
	if runner == nil || !filepath.IsAbs(options.RequestPath) || !filepath.IsAbs(options.CredentialPath) || !filepath.IsAbs(options.ResultPath) {
		return domain.NewError(domain.CodeValidation, errors.New("runner file execution paths are invalid"))
	}
	requestContents, err := readBoundedFile(options.RequestPath, maxFileRequestBytes)
	if err != nil {
		return err
	}
	var request planner.SourceRequest
	if err := decodeSingleJSON(requestContents, &request); err != nil || request.Credential != "" {
		return domain.NewError(domain.CodeValidation, errors.New("runner request file is invalid or contains a credential"))
	}
	waitTimeout := options.CredentialWaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Second
	}
	credential, err := waitForCredential(ctx, options.CredentialPath, waitTimeout)
	if err != nil {
		return err
	}
	request.Credential = credential
	evidence, executeErr := runner.IndexSource(ctx, request)
	request.Credential = ""
	envelope := ResultEnvelope{Version: ResultEnvelopeVersion, Evidence: evidence}
	if executeErr != nil {
		envelope.Evidence = planner.SourceEvidence{}
		envelope.Code = domain.CodeOf(executeErr)
		envelope.Message = safeFileResultMessage(envelope.Code)
	}
	contents, err := EncodeResult(envelope)
	if err != nil {
		return err
	}
	return writeAtomic(options.ResultPath, contents, 0o660)
}

func waitForCredential(ctx context.Context, path string, timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		contents, err := readBoundedFile(path, maxCredentialBytes)
		if err == nil {
			credential := strings.TrimSpace(string(contents))
			clear(contents)
			if credential == "" {
				return "", domain.NewError(domain.CodeAuth, errors.New("runner credential file is empty"))
			}
			_ = os.Remove(path)
			return credential, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", domain.NewError(domain.CodeBusy, errors.New("runner credential wait was cancelled"))
		case <-deadline.C:
			return "", domain.NewError(domain.CodeBusy, errors.New("runner credential file did not appear before the deadline"))
		case <-ticker.C:
		}
	}
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner file exceeds the size limit"))
	}
	return contents, nil
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

func writeAtomic(path string, contents []byte, mode os.FileMode) (err error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".runner-result.tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err = temporary.Write(contents); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func safeFileResultMessage(code domain.ErrorCode) string {
	switch code {
	case domain.CodeBusy:
		return "runner execution timed out or was cancelled"
	case domain.CodeValidation:
		return "runner request is invalid"
	case domain.CodeCoverageIncomplete:
		return "source indexing evidence is incomplete"
	default:
		return "runner execution failed"
	}
}

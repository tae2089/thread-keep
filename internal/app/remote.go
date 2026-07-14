package app

import (
	"context"
	"errors"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/tae2089/thread-keep/internal/store"
)

func (s *Service) AddRemote(ctx context.Context, name, path string) (domain.Remote, error) {
	resolved, _, err := remote.NormalizeAddress(path)
	if err != nil {
		return domain.Remote{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.Remote{}, err
	}
	return contextStore.AddRemote(ctx, domain.Remote{Name: name, Path: resolved})
}

func (s *Service) Remotes(ctx context.Context) ([]domain.Remote, error) {
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	return contextStore.Remotes(ctx)
}

func (s *Service) PushRemote(ctx context.Context, name string) (domain.RemoteSyncResult, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	local, err := contextStore.ContextRef(ctx, key)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if local.CommitID == "" {
		return domain.RemoteSyncResult{}, domain.NewError(domain.CodeValidation, errors.New("commit local context before pushing"))
	}
	configured, err := contextStore.Remote(ctx, name)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	transport, err := remote.Dial(configured.Path)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	remoteRef, err := transport.ReadRef(ctx, key.RefName)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if remoteRef.CommitID == local.CommitID {
		if remoteRef.SourceSHA != local.SourceSHA {
			return domain.RemoteSyncResult{}, domain.NewError(domain.CodeValidation, errors.New("remote ref source does not match its local immutable object"))
		}
		if err := contextStore.RecordRemoteRef(ctx, domain.RemoteRef{RemoteName: configured.Name, RefName: key.RefName, CommitID: remoteRef.CommitID, SourceSHA: remoteRef.SourceSHA, Version: remoteRef.Version}); err != nil {
			return domain.RemoteSyncResult{}, err
		}
		return domain.RemoteSyncResult{RemoteName: configured.Name, RefName: key.RefName, LocalTip: local.CommitID, RemoteTip: remoteRef.CommitID, TrackingTip: remoteRef.CommitID, Outcome: "up_to_date"}, nil
	}
	chain, err := contextStore.ReadObjectChain(local.CommitID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if remoteRef.CommitID != "" {
		remoteObject, found := findObject(chain, remoteRef.CommitID)
		if !found {
			return domain.RemoteSyncResult{}, domain.NewError(domain.CodeRemoteConflict, errors.New("remote context ref does not fast-forward to the local context ref"))
		}
		if remoteObject.SourceSHA != remoteRef.SourceSHA {
			return domain.RemoteSyncResult{}, domain.NewError(domain.CodeValidation, errors.New("remote ref source does not match its immutable object"))
		}
	}
	published := 0
	for _, object := range chain {
		created, err := transport.PublishObject(ctx, object.ID, object.Contents)
		if err != nil {
			return domain.RemoteSyncResult{}, err
		}
		if created {
			published++
		}
	}
	confirmed, err := transport.CompareAndSwapRef(ctx, key.RefName, remoteRef, remote.Ref{RefName: key.RefName, CommitID: local.CommitID, SourceSHA: local.SourceSHA, Version: remoteRef.Version + 1})
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if err := contextStore.RecordRemoteRef(ctx, domain.RemoteRef{RemoteName: configured.Name, RefName: key.RefName, CommitID: confirmed.CommitID, SourceSHA: confirmed.SourceSHA, Version: confirmed.Version}); err != nil {
		return domain.RemoteSyncResult{}, err
	}
	return domain.RemoteSyncResult{RemoteName: configured.Name, RefName: key.RefName, LocalTip: local.CommitID, RemoteTip: confirmed.CommitID, TrackingTip: confirmed.CommitID, Outcome: "pushed", TransferredObjects: published}, nil
}

func (s *Service) FetchRemote(ctx context.Context, name string) (domain.RemoteSyncResult, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	result, _, err := s.fetchRemote(ctx, contextStore, key, name)
	return result, err
}

func (s *Service) PullRemote(ctx context.Context, name string) (domain.RemoteSyncResult, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	pending, err := contextStore.PendingNotes(ctx, key)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if len(pending) != 0 {
		return domain.RemoteSyncResult{}, domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before pulling"))
	}
	result, fetched, err := s.fetchRemote(ctx, contextStore, key, name)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if fetched.Remote.CommitID == "" || fetched.Remote.CommitID == fetched.Local.CommitID {
		result.Outcome = "up_to_date"
		return result, nil
	}
	if fetched.Remote.SourceSHA != key.SourceSHA {
		return domain.RemoteSyncResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("remote context tip does not match the current Git source"))
	}
	fastForward, err := contextStore.IsAncestor(fetched.Remote.CommitID, fetched.Local.CommitID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if !fastForward {
		return domain.RemoteSyncResult{}, domain.NewError(domain.CodeRemoteConflict, errors.New("remote context does not fast-forward the local context ref"))
	}
	if err := s.requireCommitSourceState(ctx, state); err != nil {
		return domain.RemoteSyncResult{}, err
	}
	if err := contextStore.FastForward(ctx, store.FastForwardInput{Key: key, Expected: fetched.Local, Next: fetched.Remote}); err != nil {
		return domain.RemoteSyncResult{}, err
	}
	result.Outcome = "pulled"
	return result, nil
}

func (s *Service) fetchRemote(ctx context.Context, contextStore *store.Store, key domain.WorkingSetKey, name string) (domain.RemoteSyncResult, fetchedRemote, error) {
	configured, err := contextStore.Remote(ctx, name)
	if err != nil {
		return domain.RemoteSyncResult{}, fetchedRemote{}, err
	}
	local, err := contextStore.ContextRef(ctx, key)
	if err != nil {
		return domain.RemoteSyncResult{}, fetchedRemote{}, err
	}
	transport, err := remote.Dial(configured.Path)
	if err != nil {
		return domain.RemoteSyncResult{}, fetchedRemote{}, err
	}
	remoteRef, err := transport.ReadRef(ctx, key.RefName)
	if err != nil {
		return domain.RemoteSyncResult{}, fetchedRemote{}, err
	}
	result := domain.RemoteSyncResult{RemoteName: configured.Name, RefName: key.RefName, LocalTip: local.CommitID, RemoteTip: remoteRef.CommitID, Outcome: "empty"}
	if remoteRef.CommitID == "" {
		return result, fetchedRemote{Local: local, Remote: domain.ContextRef{RefName: key.RefName}}, nil
	}
	imported, err := contextStore.ImportObjectChain(ctx, remoteRef.CommitID, key.RepositoryID, key.RefName, transport)
	if err != nil {
		return domain.RemoteSyncResult{}, fetchedRemote{}, err
	}
	if imported.SourceSHA != remoteRef.SourceSHA {
		return domain.RemoteSyncResult{}, fetchedRemote{}, domain.NewError(domain.CodeValidation, errors.New("remote ref source does not match its immutable object tip"))
	}
	if err := contextStore.RecordRemoteRef(ctx, domain.RemoteRef{RemoteName: configured.Name, RefName: key.RefName, CommitID: remoteRef.CommitID, SourceSHA: remoteRef.SourceSHA, Version: remoteRef.Version}); err != nil {
		return domain.RemoteSyncResult{}, fetchedRemote{}, err
	}
	result.TrackingTip = remoteRef.CommitID
	result.Outcome = "fetched"
	result.TransferredObjects = imported.Count
	return result, fetchedRemote{Local: local, Remote: domain.ContextRef{RefName: key.RefName, CommitID: remoteRef.CommitID, SourceSHA: remoteRef.SourceSHA, Version: remoteRef.Version}, Count: imported.Count}, nil
}

func findObject(objects []store.ObjectRecord, id string) (store.ObjectRecord, bool) {
	for _, object := range objects {
		if object.ID == id {
			return object, true
		}
	}
	return store.ObjectRecord{}, false
}

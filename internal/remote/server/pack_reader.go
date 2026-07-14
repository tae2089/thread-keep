package server

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

type packDecoder interface {
	DecodeAll(input, destination []byte) ([]byte, error)
	Close()
}

type packReaderOps struct {
	readRange            func(directory, packName string, entry packEntry) ([]byte, error)
	newPlainDecoder      func() (packDecoder, error)
	newDictionaryDecoder func(dictionary []byte) (packDecoder, error)
}

type packReadState struct {
	plainDecoder      packDecoder
	dictionary        []byte
	dictionaryDecoder packDecoder
}

type packReadSession struct {
	directory string
	indexes   []packIndex
	states    map[string]*packReadState
	ops       packReaderOps
}

func newPackReadSession(directory string, indexes []packIndex) *packReadSession {
	return &packReadSession{
		directory: directory,
		indexes:   indexes,
		states:    make(map[string]*packReadState),
		ops:       defaultPackReaderOps(),
	}
}

func defaultPackReaderOps() packReaderOps {
	return packReaderOps{
		readRange: readPackRange,
		newPlainDecoder: func() (packDecoder, error) {
			return zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxObjectRequestBytes))
		},
		newDictionaryDecoder: func(dictionary []byte) (packDecoder, error) {
			return zstd.NewReader(nil, zstd.WithDecoderDictRaw(0, dictionary), zstd.WithDecoderMaxMemory(maxObjectRequestBytes))
		},
	}
}

func (s *packReadSession) readObject(objectID string) ([]byte, bool, error) {
	for _, index := range s.indexes {
		entry, ok := index.Objects[objectID]
		if !ok {
			continue
		}
		contents, err := s.readEntry(index, objectID, entry)
		if err != nil {
			return nil, false, err
		}
		return contents, true, nil
	}
	return nil, false, nil
}

func (s *packReadSession) readFromIndex(index packIndex, objectID string) ([]byte, bool, error) {
	entry, ok := index.Objects[objectID]
	if !ok {
		return nil, false, nil
	}
	contents, err := s.readEntry(index, objectID, entry)
	if err != nil {
		return nil, false, err
	}
	return contents, true, nil
}

func (s *packReadSession) readEntry(index packIndex, objectID string, entry packEntry) ([]byte, error) {
	compressed, err := s.ops.readRange(s.directory, index.name, entry)
	if err != nil {
		return nil, err
	}
	decoder, err := s.decoderFor(index, objectID)
	if err != nil {
		return nil, err
	}
	contents, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("decompress pack entry for object %s: %w", objectID, err))
	}
	if err := remote.ValidateObjectBytes(objectID, contents); err != nil {
		return nil, err
	}
	return contents, nil
}

func (s *packReadSession) decoderFor(index packIndex, objectID string) (packDecoder, error) {
	state := s.states[index.name]
	if state == nil {
		state = &packReadState{}
		s.states[index.name] = state
	}
	if objectID == index.Dictionary || index.Dictionary == "" {
		return s.plainDecoder(state)
	}
	if state.dictionaryDecoder != nil {
		return state.dictionaryDecoder, nil
	}
	dictionaryEntry, ok := index.Objects[index.Dictionary]
	if !ok {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("pack %s is missing its dictionary entry", index.name))
	}
	compressed, err := s.ops.readRange(s.directory, index.name, dictionaryEntry)
	if err != nil {
		return nil, err
	}
	plainDecoder, err := s.plainDecoder(state)
	if err != nil {
		return nil, err
	}
	dictionary, err := plainDecoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("decompress pack entry for object %s: %w", index.Dictionary, err))
	}
	if err := remote.ValidateObjectBytes(index.Dictionary, dictionary); err != nil {
		return nil, err
	}
	decoder, err := s.ops.newDictionaryDecoder(dictionary)
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack decoder: %w", err))
	}
	state.dictionary = dictionary
	state.dictionaryDecoder = decoder
	return decoder, nil
}

func (s *packReadSession) plainDecoder(state *packReadState) (packDecoder, error) {
	if state.plainDecoder != nil {
		return state.plainDecoder, nil
	}
	decoder, err := s.ops.newPlainDecoder()
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack decoder: %w", err))
	}
	state.plainDecoder = decoder
	return decoder, nil
}

func (s *packReadSession) Close() {
	for _, state := range s.states {
		if state.dictionaryDecoder != nil {
			state.dictionaryDecoder.Close()
			state.dictionaryDecoder = nil
		}
		if state.plainDecoder != nil {
			state.plainDecoder.Close()
			state.plainDecoder = nil
		}
		state.dictionary = nil
	}
}

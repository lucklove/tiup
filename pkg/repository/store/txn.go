// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"time"

	cjson "github.com/gibson042/canonicaljson-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/tiup/pkg/logger/log"
	"github.com/pingcap/tiup/pkg/repository/v1manifest"
	"github.com/pingcap/tiup/pkg/utils"
)

var (
	// ErrorFsCommitConflict indicates concurrent writing file
	ErrorFsCommitConflict = errors.New("conflict on fs commit")
)

type localTxn struct {
	syncer   Syncer
	store    *localStore
	root     string
	begin    time.Time
	accessed map[string]*time.Time
}

func newLocalTxn(store *localStore) (*localTxn, error) {
	syncer := newFsSyncer(path.Join(store.root, "commits"))
	root, err := ioutil.TempDir("/tmp", "tiup-commit-*")
	if err != nil {
		return nil, err
	}
	txn := &localTxn{
		syncer:   syncer,
		store:    store,
		root:     root,
		begin:    time.Now(),
		accessed: make(map[string]*time.Time),
	}

	return txn, nil
}

// Write implements FsTxn
func (t *localTxn) Write(filename string, reader io.Reader) error {
	filepath := path.Join(t.root, filename)
	file, err := os.Create(filepath)
	if err != nil {
		return errors.Annotate(err, "create file")
	}
	defer file.Close()

	_, err = io.Copy(file, reader)
	return err
}

// Read implements FsTxn
func (t *localTxn) Read(filename string) (io.ReadCloser, error) {
	filepath := t.store.path(filename)
	if utils.IsExist(path.Join(t.root, filename)) {
		filepath = path.Join(t.root, filename)
	}

	return os.Open(filepath)
}

func (t *localTxn) WriteManifest(filename string, manifest *v1manifest.Manifest) error {
	t.access(filename)
	filepath := path.Join(t.root, filename)
	file, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer file.Close()

	bytes, err := cjson.Marshal(manifest)
	if err != nil {
		return err
	}

	if _, err = file.Write(bytes); err != nil {
		return err
	}

	return nil
}

func (t *localTxn) ReadManifest(filename string, role v1manifest.ValidManifest) (*v1manifest.Manifest, error) {
	t.access(filename)
	filepath := t.store.path(filename)
	if utils.IsExist(path.Join(t.root, filename)) {
		filepath = path.Join(t.root, filename)
	}
	var wc io.ReadCloser
	if file, err := os.Open(filepath); err == nil {
		wc = file
	} else if os.IsNotExist(err) && t.store.upstream != "" {
		url := fmt.Sprintf("%s/%s", t.store.upstream, filename)
		if resp, err := http.Get(url); err == nil {
			wc = resp.Body
		} else {
			return nil, errors.Annotatef(err, "fetch %s", url)
		}
	} else {
		log.Errorf("Error on read manifest: %s, upstream: %s", err.Error(), t.store.upstream)
		return nil, errors.Annotate(err, "open file")
	}
	defer wc.Close()

	return v1manifest.ReadManifest(wc, role, nil)
}

func (t *localTxn) ResetManifest() error {
	for file := range t.accessed {
		fp := path.Join(t.root, file)
		if utils.IsExist(fp) {
			if err := os.Remove(fp); err != nil {
				return err
			}
		}
	}
	t.begin = time.Now()
	return nil
}

func (t *localTxn) Stat(filename string) (os.FileInfo, error) {
	t.access(filename)
	filepath := t.store.path(filename)
	if utils.IsExist(path.Join(t.root, filename)) {
		filepath = path.Join(t.root, filename)
	}
	return os.Stat(filepath)
}

func (t *localTxn) Commit() error {
	t.store.lock()
	defer t.store.unlock()

	if err := t.checkConflict(); err != nil {
		return err
	}

	files, err := ioutil.ReadDir(t.root)
	if err != nil {
		return err
	}

	for _, f := range files {
		if err := utils.Copy(path.Join(t.root, f.Name()), t.store.path(f.Name())); err != nil {
			return err
		}
	}

	if err := t.syncer.Sync(t.root); err != nil {
		return err
	}

	return t.release()
}

func (t *localTxn) Rollback() error {
	return t.release()
}

func (t *localTxn) checkConflict() error {
	for file := range t.accessed {
		mt, err := t.store.last(file)
		if err != nil {
			return err
		}
		if mt != nil && mt.After(*t.first(file)) {
			return ErrorFsCommitConflict
		}
	}
	return nil
}

func (t *localTxn) access(filename string) {
	// Use the earliest time
	if t.accessed[filename] != nil {
		return
	}

	at := time.Now()
	t.accessed[filename] = &at
}

// Returns the first access time
func (t *localTxn) first(filename string) *time.Time {
	return t.accessed[filename]
}

func (t *localTxn) release() error {
	return os.RemoveAll(t.root)
}

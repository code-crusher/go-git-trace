package git

import (
	"crypto"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	fixtures "github.com/go-git/go-git-fixtures/v6"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/plumbing/hash"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

type PruneSuite struct {
	BaseSuite
}

func TestPruneSuite(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(PruneSuite))
}

func (s *PruneSuite) testPrune(deleteTime time.Time) {
	srcFs, err := fixtures.ByTag("unpacked").One().DotGit()
	s.Require().NoError(err)
	var sto storage.Storer = filesystem.NewStorage(srcFs, cache.NewObjectLRUDefault())

	los := sto.(storer.LooseObjectStorer)
	s.NotNil(los)

	count := 0
	err = los.ForEachObjectHash(func(_ plumbing.Hash) error {
		count++
		return nil
	})
	s.NoError(err)

	r, err := Open(sto, srcFs)
	s.NoError(err)
	s.NotNil(r)
	defer func() { _ = r.Close() }()

	// Remove a branch so we can prune some objects.
	err = sto.RemoveReference(plumbing.ReferenceName("refs/heads/v4"))
	s.NoError(err)
	err = sto.RemoveReference(plumbing.ReferenceName("refs/remotes/origin/v4"))
	s.NoError(err)

	err = r.Prune(PruneOptions{
		OnlyObjectsOlderThan: deleteTime,
		Handler:              r.DeleteObject,
	})
	s.NoError(err)

	newCount := 0
	err = los.ForEachObjectHash(func(_ plumbing.Hash) error {
		newCount++
		return nil
	})
	s.NoError(err)

	if deleteTime.IsZero() {
		s.True(newCount < count)
	} else {
		// Assume a delete time older than any of the objects was passed in.
		s.Equal(count, newCount)
	}
}

func (s *PruneSuite) TestPrune() {
	s.testPrune(time.Time{})
}

func (s *PruneSuite) TestPruneWithNoDelete() {
	s.testPrune(time.Unix(0, 1))
}

func newPruneSymrefRepository(t *testing.T) (*Repository, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	st := filesystem.NewStorage(memfs.New(), cache.NewObjectLRUDefault())
	r, err := Init(st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	tree := st.NewEncodedObject()
	require.NoError(t, (&object.Tree{}).Encode(tree))
	treeHash, err := st.SetEncodedObject(tree)
	require.NoError(t, err)
	commit := st.NewEncodedObject()
	require.NoError(t, (&object.Commit{TreeHash: treeHash, Message: "reachable"}).Encode(commit))
	commitHash, err := st.SetEncodedObject(commit)
	require.NoError(t, err)
	return r, commitHash, treeHash
}

func (s *PruneSuite) TestPrunePreservesObjectsThroughRootSymref() {
	r, commit, tree := newPruneSymrefRepository(s.T())
	s.Require().NoError(r.Storer.SetReference(plumbing.NewHashReference("ORIG_HEAD", commit)))
	s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference("refs/heads/main", "ORIG_HEAD")))
	s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")))

	s.Require().NoError(r.Prune(PruneOptions{Handler: r.DeleteObject}))
	s.Require().NoError(r.Storer.HasEncodedObject(commit))
	s.Require().NoError(r.Storer.HasEncodedObject(tree))
}

func (s *PruneSuite) TestPruneAllowsUnbornAndDanglingSymrefs() {
	r, commit, tree := newPruneSymrefRepository(s.T())
	s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference("refs/heads/dangling", "refs/heads/missing")))
	s.Require().NoError(r.Prune(PruneOptions{Handler: r.DeleteObject}))
	s.Require().ErrorIs(r.Storer.HasEncodedObject(commit), plumbing.ErrObjectNotFound)
	s.Require().ErrorIs(r.Storer.HasEncodedObject(tree), plumbing.ErrObjectNotFound)
	s.Require().NoError(r.Prune(PruneOptions{Handler: r.DeleteObject}))
}

type pruneReferenceReadError struct {
	*filesystem.Storage
	err error
}

func (s pruneReferenceReadError) Reference(plumbing.ReferenceName) (*plumbing.Reference, error) {
	return nil, s.err
}

func (s *PruneSuite) TestPruneStopsBeforeDeletingOnSymrefReadError() {
	for _, want := range []error{io.ErrUnexpectedEOF, plumbing.ErrInvalidReferenceName} {
		s.Run(want.Error(), func() {
			r, commit, tree := newPruneSymrefRepository(s.T())
			r.Storer = pruneReferenceReadError{r.Storer.(*filesystem.Storage), want}
			called := false
			err := r.Prune(PruneOptions{Handler: func(hash plumbing.Hash) error {
				called = true
				return r.DeleteObject(hash)
			}})
			s.Require().ErrorIs(err, want)
			s.Require().False(called)
			s.Require().NoError(r.Storer.HasEncodedObject(commit))
			s.Require().NoError(r.Storer.HasEncodedObject(tree))
		})
	}
}

func (s *PruneSuite) TestPruneStopsBeforeDeletingOnCyclicSymref() {
	r, commit, tree := newPruneSymrefRepository(s.T())
	s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/loop")))
	s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference("refs/heads/loop", plumbing.HEAD)))
	called := false
	err := r.Prune(PruneOptions{Handler: func(plumbing.Hash) error {
		called = true
		return errors.New("destructive handler called")
	}})
	s.Require().ErrorIs(err, storer.ErrMaxResolveRecursion)
	s.Require().False(called)
	s.Require().NoError(r.Storer.HasEncodedObject(commit))
	s.Require().NoError(r.Storer.HasEncodedObject(tree))
}

type pruneRootRefOpenError struct {
	billy.Filesystem
	opened bool
}

func (s *pruneRootRefOpenError) Open(name string) (billy.File, error) {
	if s.Join(name) == "ORIG_HEAD" {
		s.opened = true
		return nil, io.ErrUnexpectedEOF
	}
	return s.Filesystem.Open(name)
}

func (s *PruneSuite) TestMaintenanceStopsOnSymbolicTargetFileReadError() {
	for _, operation := range []string{"prune", "repack"} {
		s.Run(operation, func() {
			r, commit, tree := newPruneSymrefRepository(s.T())
			s.Require().NoError(r.Storer.SetReference(plumbing.NewHashReference("ORIG_HEAD", commit)))
			s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference("refs/heads/main", "ORIG_HEAD")))
			s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")))
			fs := &pruneRootRefOpenError{Filesystem: r.Storer.(*filesystem.Storage).Filesystem()}
			r.Storer = filesystem.NewStorage(fs, cache.NewObjectLRUDefault())
			called := false
			var err error
			if operation == "prune" {
				err = r.Prune(PruneOptions{Handler: func(hash plumbing.Hash) error {
					called = true
					return r.DeleteObject(hash)
				}})
			} else {
				err = r.RepackObjects(&RepackConfig{})
			}
			s.Require().True(fs.opened)
			s.Require().ErrorIs(err, io.ErrUnexpectedEOF)
			s.Require().False(called)
			s.Require().NoError(r.Storer.HasEncodedObject(commit))
			s.Require().NoError(r.Storer.HasEncodedObject(tree))
			packs, err := r.Storer.(storer.PackedObjectStorer).ObjectPacks()
			s.Require().NoError(err)
			s.Require().Empty(packs)
		})
	}
}

type packTempFileCloseError struct {
	billy.Filesystem
	err      error
	injected bool
}

func (s *packTempFileCloseError) TempFile(dir, prefix string) (billy.File, error) {
	f, err := s.Filesystem.TempFile(dir, prefix)
	if err != nil || !strings.HasPrefix(prefix, "tmp_pack_") {
		return f, err
	}
	s.injected = true
	return &closeErrorFile{File: f, err: s.err}, nil
}

type closeErrorFile struct {
	billy.File
	err error
}

func (f *closeErrorFile) Close() error {
	_ = f.File.Close()
	return f.err
}

func (s *PruneSuite) TestRepackKeepsObjectsWhenNewPackFailsToPublish() {
	r, commit, tree := newPruneSymrefRepository(s.T())
	s.Require().NoError(r.Storer.SetReference(plumbing.NewHashReference("refs/heads/main", commit)))
	s.Require().NoError(r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")))

	publishErr := errors.New("pack publish failure")
	original := r.Storer.(*filesystem.Storage)
	s.T().Cleanup(func() { s.Require().NoError(original.Close()) })
	fs := &packTempFileCloseError{Filesystem: original.Filesystem(), err: publishErr}
	r.Storer = filesystem.NewStorage(fs, cache.NewObjectLRUDefault())

	err := r.RepackObjects(&RepackConfig{})
	s.Require().True(fs.injected)
	s.Require().ErrorIs(err, publishErr)

	_, err = r.CommitObject(commit)
	s.Require().NoError(err)
	_, err = r.TreeObject(tree)
	s.Require().NoError(err)
	packs, err := r.Storer.(storer.PackedObjectStorer).ObjectPacks()
	s.Require().NoError(err)
	s.Require().Empty(packs)
}

func newMaintenanceStorage(t *testing.T) (*Repository, *filesystem.Storage) {
	t.Helper()
	st := filesystem.NewStorage(memfs.New(), cache.NewObjectLRUDefault())
	r, err := Init(st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r, st
}

func storeMaintenanceBlob(t *testing.T, st *filesystem.Storage, content string) plumbing.Hash {
	t.Helper()
	obj := st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	h, err := st.SetEncodedObject(obj)
	require.NoError(t, err)
	return h
}

func storeMaintenanceObject(t *testing.T, st *filesystem.Storage, o object.Object) plumbing.Hash {
	t.Helper()
	obj := st.NewEncodedObject()
	require.NoError(t, o.Encode(obj))
	h, err := st.SetEncodedObject(obj)
	require.NoError(t, err)
	return h
}

func addMaintenanceCommit(t *testing.T, st *filesystem.Storage, entries ...object.TreeEntry) plumbing.Hash {
	t.Helper()
	tree := st.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: entries}).Encode(tree))
	treeHash, err := st.SetEncodedObject(tree)
	require.NoError(t, err)
	commit := st.NewEncodedObject()
	require.NoError(t, (&object.Commit{TreeHash: treeHash, Message: "reachable"}).Encode(commit))
	commitHash, err := st.SetEncodedObject(commit)
	require.NoError(t, err)
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/main", commitHash)))
	return commitHash
}

func runMaintenance(r *Repository, operation string) error {
	if operation == "prune" {
		return r.Prune(PruneOptions{Handler: r.DeleteObject})
	}
	return r.RepackObjects(&RepackConfig{})
}

// packedObjects returns the hashes recorded in the indexes of the
// repository's packs. RepackObjects leaves unreachable loose objects alone,
// so the pack contents, not HasEncodedObject, show what it considered
// reachable.
func packedObjects(t *testing.T, st *filesystem.Storage) map[plumbing.Hash]bool {
	t.Helper()
	dg := dotgit.New(st.Filesystem())
	packs, err := dg.ObjectPacks()
	require.NoError(t, err)
	packed := make(map[plumbing.Hash]bool)
	for _, pack := range packs {
		f, err := dg.ObjectPackIdx(pack)
		require.NoError(t, err)
		idx := idxfile.NewMemoryIndex(crypto.SHA1.Size())
		require.NoError(t, idxfile.NewDecoder(f, hash.New(crypto.SHA1)).Decode(idx))
		require.NoError(t, f.Close())
		entries, err := idx.Entries()
		require.NoError(t, err)
		for {
			e, err := entries.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			packed[e.Hash] = true
		}
		require.NoError(t, entries.Close())
	}
	return packed
}

// assertReachable checks that maintenance kept hash. Prune deletes the loose
// copy of an unreachable object outright, while repack writes only what it
// reached into the new pack and leaves unreachable loose objects in place, so
// the two operations have to be inspected differently.
func assertReachable(t *testing.T, r *Repository, st *filesystem.Storage, operation string, h plumbing.Hash) {
	t.Helper()
	if operation == "prune" {
		require.NoError(t, r.Storer.HasEncodedObject(h))
		return
	}
	require.Contains(t, packedObjects(t, st), h)
}

func assertUnreachable(t *testing.T, r *Repository, st *filesystem.Storage, operation string, h plumbing.Hash) {
	t.Helper()
	if operation == "prune" {
		require.ErrorIs(t, r.Storer.HasEncodedObject(h), plumbing.ErrObjectNotFound)
		return
	}
	require.NotContains(t, packedObjects(t, st), h)
}

// Tree entries for regular, executable and symlink files all reach a blob.
// The symlink entry does not take the tree shortcut for plain files, so the
// walk used to reject its blob as an unknown object (#2540).
func (s *PruneSuite) TestMaintenanceKeepsFileBlobsReachable() {
	for _, mode := range []filemode.FileMode{filemode.Regular, filemode.Executable, filemode.Symlink} {
		for _, operation := range []string{"prune", "repack"} {
			s.Run(operation+" "+mode.String(), func() {
				r, st := newMaintenanceStorage(s.T())
				blob := storeMaintenanceBlob(s.T(), st, "target")
				addMaintenanceCommit(s.T(), st, object.TreeEntry{Name: "entry", Mode: mode, Hash: blob})

				s.Require().NoError(runMaintenance(r, operation))
				assertReachable(s.T(), r, st, operation, blob)
			})
		}
	}
}

// A gitlink names a commit in the submodule's history, which a superproject
// need not contain. Walking it made maintenance fail when the commit was
// absent and retain it when it was present (#2540).
func (s *PruneSuite) TestMaintenanceSkipsSubmoduleGitlink() {
	for _, operation := range []string{"prune", "repack"} {
		s.Run(operation, func() {
			r, st := newMaintenanceStorage(s.T())
			gitlink := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			addMaintenanceCommit(s.T(), st, object.TreeEntry{Name: "sub", Mode: filemode.Submodule, Hash: gitlink})

			s.Require().NoError(runMaintenance(r, operation))
			s.Require().ErrorIs(r.Storer.HasEncodedObject(gitlink), plumbing.ErrObjectNotFound)
		})
	}
}

func (s *PruneSuite) TestMaintenanceDoesNotRetainLocalGitlinkTarget() {
	for _, operation := range []string{"prune", "repack"} {
		s.Run(operation, func() {
			r, st := newMaintenanceStorage(s.T())
			tree := st.NewEncodedObject()
			s.Require().NoError((&object.Tree{}).Encode(tree))
			treeHash, err := st.SetEncodedObject(tree)
			s.Require().NoError(err)
			target := storeMaintenanceObject(s.T(), st, &object.Commit{TreeHash: treeHash, Message: "submodule history"})
			addMaintenanceCommit(s.T(), st, object.TreeEntry{Name: "sub", Mode: filemode.Submodule, Hash: target})

			s.Require().NoError(runMaintenance(r, operation))
			assertUnreachable(s.T(), r, st, operation, target)
		})
	}
}

// An annotated tag may target a blob directly, which reaches the same
// terminal blob case as a symlink entry (#2540).
func (s *PruneSuite) TestMaintenanceKeepsBlobTargetedByAnnotatedTag() {
	for _, operation := range []string{"prune", "repack"} {
		s.Run(operation, func() {
			r, st := newMaintenanceStorage(s.T())
			blob := storeMaintenanceBlob(s.T(), st, "tagged")
			tag := &object.Tag{
				Name:       "blob-tag",
				Target:     blob,
				TargetType: plumbing.BlobObject,
				Message:    "tag a blob",
			}
			tagHash := storeMaintenanceObject(s.T(), st, tag)
			s.Require().NoError(st.SetReference(plumbing.NewHashReference("refs/tags/blob-tag", tagHash)))

			s.Require().NoError(runMaintenance(r, operation))
			assertReachable(s.T(), r, st, operation, blob)
			assertReachable(s.T(), r, st, operation, tagHash)
		})
	}
}

// A symlink whose blob is absent is corruption, not a leaf to accept, so
// maintenance must fail before its handler can delete anything.
func (s *PruneSuite) TestMaintenanceStopsOnMissingSymlinkBlob() {
	for _, operation := range []string{"prune", "repack"} {
		s.Run(operation, func() {
			r, st := newMaintenanceStorage(s.T())
			missing := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
			addMaintenanceCommit(s.T(), st, object.TreeEntry{Name: "link", Mode: filemode.Symlink, Hash: missing})
			unrelated := storeMaintenanceBlob(s.T(), st, "unrelated")

			s.Require().ErrorIs(runMaintenance(r, operation), plumbing.ErrObjectNotFound)
			s.Require().NoError(r.Storer.HasEncodedObject(unrelated))
			packs, err := r.Storer.(storer.PackedObjectStorer).ObjectPacks()
			s.Require().NoError(err)
			s.Require().Empty(packs)
		})
	}
}

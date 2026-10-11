package transport

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/go-git/go-git/v6/utils/ioutil"
)

func receiveAtomic(t *testing.T, st storage.Storer, cmds []*packp.Command) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := ReceivePack(context.Background(), st, receivePackRequest(t, cmds, capability.Atomic),
		ioutil.WriteNopCloser(&out), &ReceivePackRequest{StatelessRPC: true})
	return out.String(), err
}

func requireRef(t *testing.T, st storage.Storer, name plumbing.ReferenceName, want plumbing.Hash) {
	t.Helper()
	ref, err := st.Reference(name)
	if want.IsZero() {
		require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "%s must not exist", name)
		return
	}
	require.NoError(t, err)
	assert.Equal(t, want, ref.Hash(), "%s", name)
}

func TestAdvertiseRefsReceivePackAtomic(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, AdvertiseRefs(context.Background(), memory.NewStorage(), &buf, ReceivePackService, false, protocol.V0))

	ar := &packp.AdvRefs{}
	require.NoError(t, ar.Decode(&buf))
	assert.True(t, ar.Capabilities.Supports(capability.Atomic))
}

func TestReceivePackAtomicAppliesEveryCommand(t *testing.T) {
	t.Parallel()

	st := memory.NewStorage()
	old := storeReceiveObject(t, st, "old")
	next := storeReceiveObject(t, st, "next")
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/main", old)))
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/gone", old)))

	out, err := receiveAtomic(t, st, []*packp.Command{
		{Name: "refs/heads/main", Old: old, New: next},
		{Name: "refs/heads/feature", Old: plumbing.ZeroHash, New: next},
		deleteCmd("refs/heads/gone", old),
	})
	require.NoError(t, err)
	assert.Equal(t, "000eunpack ok\n0017ok refs/heads/main\n001aok refs/heads/feature\n0017ok refs/heads/gone\n0000", out)

	requireRef(t, st, "refs/heads/main", next)
	requireRef(t, st, "refs/heads/feature", next)
	requireRef(t, st, "refs/heads/gone", plumbing.ZeroHash)
}

// A command that fails its checks makes git refuse the whole atomic push: no
// reference moves, the failing command reports its own reason and every other
// command reports "atomic push failure" (builtin/receive-pack.c,
// execute_commands_atomic).
func TestReceivePackAtomicRefusesWholePush(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		bad     func(old, next, stale plumbing.Hash) *packp.Command
		wantErr error
	}{
		{
			name: "stale update",
			bad: func(_, next, stale plumbing.Hash) *packp.Command {
				return &packp.Command{Name: "refs/heads/bad", Old: stale, New: next}
			},
			wantErr: storage.ErrReferenceHasChanged,
		},
		{
			name:    "stale delete",
			bad:     func(_, _, stale plumbing.Hash) *packp.Command { return deleteCmd("refs/heads/bad", stale) },
			wantErr: storage.ErrReferenceHasChanged,
		},
		{
			name: "create over existing",
			bad: func(_, next, _ plumbing.Hash) *packp.Command {
				return &packp.Command{Name: "refs/heads/bad", New: next}
			},
			wantErr: ErrUpdateReference,
		},
		{
			name: "missing object",
			bad: func(old, _, _ plumbing.Hash) *packp.Command {
				return &packp.Command{Name: "refs/heads/bad", Old: old, New: plumbing.NewHash(receivePackTestHash)}
			},
			wantErr: ErrMissingObject,
		},
		{
			name: "funny refname",
			bad: func(_, next, _ plumbing.Hash) *packp.Command {
				return &packp.Command{Name: "refs/heads/a..b", New: next}
			},
			wantErr: ErrFunnyRefname,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := memory.NewStorage()
			old := storeReceiveObject(t, st, "old")
			next := storeReceiveObject(t, st, "next")
			stale := storeReceiveObject(t, st, "stale")
			require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/main", old)))
			require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/gone", old)))
			require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/bad", old)))

			bad := tc.bad(old, next, stale)
			out, err := receiveAtomic(t, st, []*packp.Command{
				{Name: "refs/heads/main", Old: old, New: next},
				{Name: "refs/heads/feature", Old: plumbing.ZeroHash, New: next},
				bad,
				deleteCmd("refs/heads/gone", old),
			})
			require.ErrorIs(t, err, tc.wantErr)

			assert.Contains(t, out, "ng refs/heads/main atomic push failure\n")
			assert.Contains(t, out, "ng refs/heads/feature atomic push failure\n")
			assert.Contains(t, out, "ng "+bad.Name.String()+" "+tc.wantErr.Error()+"\n")
			assert.Contains(t, out, "ng refs/heads/gone atomic push failure\n")

			requireRef(t, st, "refs/heads/main", old)
			requireRef(t, st, "refs/heads/feature", plumbing.ZeroHash)
			requireRef(t, st, "refs/heads/gone", old)
			requireRef(t, st, "refs/heads/bad", old)
		})
	}
}

// Without the atomic capability a failing command still leaves the others
// applied, as git does (execute_commands_non_atomic).
func TestReceivePackNonAtomicKeepsGoodCommands(t *testing.T) {
	t.Parallel()

	st := memory.NewStorage()
	old := storeReceiveObject(t, st, "old")
	next := storeReceiveObject(t, st, "next")
	stale := storeReceiveObject(t, st, "stale")
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/main", old)))
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/bad", old)))

	var out bytes.Buffer
	err := ReceivePack(context.Background(), st, receivePackRequest(t, []*packp.Command{
		{Name: "refs/heads/main", Old: old, New: next},
		{Name: "refs/heads/bad", Old: stale, New: next},
	}), ioutil.WriteNopCloser(&out), &ReceivePackRequest{StatelessRPC: true})
	require.ErrorIs(t, err, storage.ErrReferenceHasChanged)
	assert.Contains(t, out.String(), "ok refs/heads/main\n")
	assert.Contains(t, out.String(), "ng refs/heads/bad ")
	requireRef(t, st, "refs/heads/main", next)
	requireRef(t, st, "refs/heads/bad", old)
}

var errInjectedWrite = errors.New("injected write failure")

// receiveFailingWriteStorage fails writes to one reference, to make an atomic
// push fail after some of its commands were already applied.
type receiveFailingWriteStorage struct {
	storage.Storer
	fail plumbing.ReferenceName
}

func (s *receiveFailingWriteStorage) SetReference(ref *plumbing.Reference) error {
	if ref.Name() == s.fail {
		return errInjectedWrite
	}
	return s.Storer.SetReference(ref)
}

func (s *receiveFailingWriteStorage) CheckAndSetReference(ref, old *plumbing.Reference) error {
	if ref.Name() == s.fail {
		return errInjectedWrite
	}
	return s.Storer.CheckAndSetReference(ref, old)
}

func (s *receiveFailingWriteStorage) RemoveReference(name plumbing.ReferenceName) error {
	if name == s.fail {
		return errInjectedWrite
	}
	return s.Storer.RemoveReference(name)
}

// A write that fails once earlier commands of an atomic push were applied
// reverts them, so the push still lands as a whole or not at all.
func TestReceivePackAtomicRevertsAppliedCommands(t *testing.T) {
	t.Parallel()

	for _, failing := range []plumbing.ReferenceName{"refs/heads/feature", "refs/heads/main", "refs/heads/gone"} {
		t.Run(failing.Short(), func(t *testing.T) {
			t.Parallel()

			base := memory.NewStorage()
			old := storeReceiveObject(t, base, "old")
			next := storeReceiveObject(t, base, "next")
			require.NoError(t, base.SetReference(plumbing.NewHashReference("refs/heads/main", old)))
			require.NoError(t, base.SetReference(plumbing.NewHashReference("refs/heads/gone", old)))
			require.NoError(t, base.SetReference(plumbing.NewHashReference("refs/heads/last", old)))
			st := &receiveFailingWriteStorage{Storer: base, fail: failing}

			cmds := []*packp.Command{
				{Name: "refs/heads/main", Old: old, New: next},
				{Name: "refs/heads/feature", Old: plumbing.ZeroHash, New: next},
				deleteCmd("refs/heads/gone", old),
				{Name: "refs/heads/last", Old: old, New: next},
			}
			out, err := receiveAtomic(t, st, cmds)
			require.ErrorIs(t, err, errInjectedWrite)

			for _, cmd := range cmds {
				reason := "atomic transaction failed"
				if cmd.Name == failing {
					reason = errInjectedWrite.Error()
				}
				assert.Contains(t, out, "ng "+cmd.Name.String()+" "+reason+"\n")
			}

			requireRef(t, base, "refs/heads/main", old)
			requireRef(t, base, "refs/heads/feature", plumbing.ZeroHash)
			requireRef(t, base, "refs/heads/gone", old)
			requireRef(t, base, "refs/heads/last", old)
		})
	}
}

// The post-receive hook only sees the commands that were applied, which for a
// refused atomic push is none of them.
func TestReceivePackAtomicFailureSkipsPostReceiveCommands(t *testing.T) {
	t.Parallel()

	st := memory.NewStorage()
	old := storeReceiveObject(t, st, "old")
	next := storeReceiveObject(t, st, "next")
	stale := storeReceiveObject(t, st, "stale")
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/main", old)))
	require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/heads/bad", old)))

	var applied []*packp.Command
	var out bytes.Buffer
	err := ReceivePack(context.Background(), st, receivePackRequest(t, []*packp.Command{
		{Name: "refs/heads/main", Old: old, New: next},
		{Name: "refs/heads/bad", Old: stale, New: next},
	}, capability.Atomic), ioutil.WriteNopCloser(&out), &ReceivePackRequest{
		StatelessRPC: true,
		Hooks: ReceivePackHooks{PostReceive: func(_ context.Context, info *PostReceiveInfo) error {
			applied = info.Commands
			return nil
		}},
	})
	require.ErrorIs(t, err, storage.ErrReferenceHasChanged)
	assert.Empty(t, applied)
	requireRef(t, st, "refs/heads/main", old)
}

var errInjectedRevert = errors.New("injected revert failure")

// receiveFailingRevertStorage fails the write to fail, and the removal of
// unremovable, which an atomic push reverting a create of that name needs.
type receiveFailingRevertStorage struct {
	receiveFailingWriteStorage
	unremovable plumbing.ReferenceName
}

func (s *receiveFailingRevertStorage) RemoveReference(name plumbing.ReferenceName) error {
	if name == s.unremovable {
		return errInjectedRevert
	}
	return s.receiveFailingWriteStorage.RemoveReference(name)
}

// A revert that fails cannot be reported on the wire, where every status line
// still carries a single-line reason. It reaches the caller instead, joined
// with the write error, as the sign that the push was left partly applied.
func TestReceivePackAtomicReportsFailedRevert(t *testing.T) {
	t.Parallel()

	base := memory.NewStorage()
	old := storeReceiveObject(t, base, "old")
	next := storeReceiveObject(t, base, "next")
	require.NoError(t, base.SetReference(plumbing.NewHashReference("refs/heads/main", old)))
	st := &receiveFailingRevertStorage{
		receiveFailingWriteStorage: receiveFailingWriteStorage{Storer: base, fail: "refs/heads/main"},
		unremovable:                "refs/heads/feature",
	}

	out, err := receiveAtomic(t, st, []*packp.Command{
		{Name: "refs/heads/feature", Old: plumbing.ZeroHash, New: next},
		{Name: "refs/heads/main", Old: old, New: next},
	})
	require.ErrorIs(t, err, errInjectedWrite)
	require.ErrorIs(t, err, errInjectedRevert)
	assert.Contains(t, err.Error(), "reverting refs/heads/feature")

	assert.Equal(t, "000eunpack ok\n"+
		"0034ng refs/heads/feature atomic transaction failed\n"+
		"002eng refs/heads/main injected write failure\n"+
		"0000", out)

	requireRef(t, base, "refs/heads/main", old)
	requireRef(t, base, "refs/heads/feature", next)
}

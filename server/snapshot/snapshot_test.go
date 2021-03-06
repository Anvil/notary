package snapshot

import (
	"bytes"
	"testing"
	"time"

	"github.com/docker/go/canonical/json"
	"github.com/docker/notary/server/storage"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/signed"
	"github.com/docker/notary/tuf/testutils"
	"github.com/stretchr/testify/require"
)

func TestSnapshotExpired(t *testing.T) {
	sn := &data.SignedSnapshot{
		Signatures: nil,
		Signed: data.Snapshot{
			SignedCommon: data.SignedCommon{Expires: time.Now().AddDate(-1, 0, 0)},
		},
	}
	require.True(t, snapshotExpired(sn), "Snapshot should have expired")
}

func TestSnapshotNotExpired(t *testing.T) {
	sn := &data.SignedSnapshot{
		Signatures: nil,
		Signed: data.Snapshot{
			SignedCommon: data.SignedCommon{Expires: time.Now().AddDate(1, 0, 0)},
		},
	}
	require.False(t, snapshotExpired(sn), "Snapshot should NOT have expired")
}

func TestGetSnapshotKeyCreate(t *testing.T) {
	store := storage.NewMemStorage()
	crypto := signed.NewEd25519()
	k, err := GetOrCreateSnapshotKey("gun", store, crypto, data.ED25519Key)
	require.Nil(t, err, "Expected nil error")
	require.NotNil(t, k, "Key should not be nil")

	k2, err := GetOrCreateSnapshotKey("gun", store, crypto, data.ED25519Key)

	require.Nil(t, err, "Expected nil error")

	// trying to get the same key again should return the same value
	require.Equal(t, k, k2, "Did not receive same key when attempting to recreate.")
	require.NotNil(t, k2, "Key should not be nil")
}

func TestGetSnapshotKeyExisting(t *testing.T) {
	store := storage.NewMemStorage()
	crypto := signed.NewEd25519()
	key, err := crypto.Create(data.CanonicalSnapshotRole, "gun", data.ED25519Key)
	require.NoError(t, err)

	store.SetKey("gun", data.CanonicalSnapshotRole, data.ED25519Key, key.Public())

	k, err := GetOrCreateSnapshotKey("gun", store, crypto, data.ED25519Key)
	require.Nil(t, err, "Expected nil error")
	require.NotNil(t, k, "Key should not be nil")
	require.Equal(t, key, k, "Did not receive same key when attempting to recreate.")
	require.NotNil(t, k, "Key should not be nil")

	k2, err := GetOrCreateSnapshotKey("gun", store, crypto, data.ED25519Key)

	require.Nil(t, err, "Expected nil error")

	// trying to get the same key again should return the same value
	require.Equal(t, k, k2, "Did not receive same key when attempting to recreate.")
	require.NotNil(t, k2, "Key should not be nil")
}

type keyStore struct {
	getCalled bool
	k         data.PublicKey
}

func (ks *keyStore) GetKey(gun, role string) (string, []byte, error) {
	defer func() { ks.getCalled = true }()
	if ks.getCalled {
		return ks.k.Algorithm(), ks.k.Public(), nil
	}
	return "", nil, &storage.ErrNoKey{}
}

func (ks keyStore) SetKey(gun, role, algorithm string, public []byte) error {
	return &storage.ErrKeyExists{}
}

// Tests the race condition where the server is being asked to generate a new key
// by 2 parallel requests and the second insert to be executed by the DB fails
// due to duplicate key (gun + role). It should then return the key added by the
// first insert.
func TestGetSnapshotKeyExistsOnSet(t *testing.T) {
	crypto := signed.NewEd25519()
	key, err := crypto.Create(data.CanonicalSnapshotRole, "gun", data.ED25519Key)
	require.NoError(t, err)
	store := &keyStore{k: key}

	k, err := GetOrCreateSnapshotKey("gun", store, crypto, data.ED25519Key)
	require.Nil(t, err, "Expected nil error")
	require.NotNil(t, k, "Key should not be nil")
	require.Equal(t, key, k, "Did not receive same key when attempting to recreate.")
	require.NotNil(t, k, "Key should not be nil")

	k2, err := GetOrCreateSnapshotKey("gun", store, crypto, data.ED25519Key)

	require.Nil(t, err, "Expected nil error")

	// trying to get the same key again should return the same value
	require.Equal(t, k, k2, "Did not receive same key when attempting to recreate.")
	require.NotNil(t, k2, "Key should not be nil")
}

// If there is no previous snapshot or the previous snapshot is corrupt, then
// even if everything else is in place, getting the snapshot fails
func TestGetSnapshotNoPreviousSnapshot(t *testing.T) {
	repo, crypto, err := testutils.EmptyRepo("gun")
	require.NoError(t, err)

	rootJSON, err := json.Marshal(repo.Root)
	require.NoError(t, err)

	for _, snapshotJSON := range [][]byte{nil, []byte("invalid JSON")} {
		store := storage.NewMemStorage()

		// so we know it's not a failure in getting root
		require.NoError(t,
			store.UpdateCurrent("gun", storage.MetaUpdate{Role: data.CanonicalRootRole, Version: 0, Data: rootJSON}))

		if snapshotJSON != nil {
			require.NoError(t,
				store.UpdateCurrent("gun",
					storage.MetaUpdate{Role: data.CanonicalSnapshotRole, Version: 0, Data: snapshotJSON}))
		}

		// create a key to be used by GetOrCreateSnapshot
		key, err := crypto.Create(data.CanonicalSnapshotRole, "gun", data.ECDSAKey)
		require.NoError(t, err)
		require.NoError(t, store.SetKey("gun", data.CanonicalSnapshotRole, key.Algorithm(), key.Public()))

		_, _, err = GetOrCreateSnapshot("gun", store, crypto)
		require.Error(t, err, "GetSnapshot should have failed")
		if snapshotJSON == nil {
			require.IsType(t, storage.ErrNotFound{}, err)
		} else {
			require.IsType(t, &json.SyntaxError{}, err)
		}
	}
}

// If there WAS a pre-existing snapshot, and it is not expired, then just return it (it doesn't
// load any other metadata that it doesn't need)
func TestGetSnapshotReturnsPreviousSnapshotIfUnexpired(t *testing.T) {
	store := storage.NewMemStorage()
	repo, crypto, err := testutils.EmptyRepo("gun")
	require.NoError(t, err)

	snapshotJSON, err := json.Marshal(repo.Snapshot)
	require.NoError(t, err)

	require.NoError(t, store.UpdateCurrent("gun",
		storage.MetaUpdate{Role: data.CanonicalSnapshotRole, Version: 0, Data: snapshotJSON}))

	// test when db is missing the role data (no root)
	_, gottenSnapshot, err := GetOrCreateSnapshot("gun", store, crypto)
	require.NoError(t, err, "GetSnapshot should not have failed")
	require.True(t, bytes.Equal(snapshotJSON, gottenSnapshot))
}

func TestGetSnapshotOldSnapshotExpired(t *testing.T) {
	store := storage.NewMemStorage()
	repo, crypto, err := testutils.EmptyRepo("gun")
	require.NoError(t, err)

	rootJSON, err := json.Marshal(repo.Root)
	require.NoError(t, err)

	// create an expired snapshot
	_, err = repo.SignSnapshot(time.Now().AddDate(-1, -1, -1))
	require.True(t, repo.Snapshot.Signed.Expires.Before(time.Now()))
	require.NoError(t, err)
	snapshotJSON, err := json.Marshal(repo.Snapshot)
	require.NoError(t, err)

	// set all the metadata
	require.NoError(t, store.UpdateCurrent("gun",
		storage.MetaUpdate{Role: data.CanonicalRootRole, Version: 0, Data: rootJSON}))
	require.NoError(t, store.UpdateCurrent("gun",
		storage.MetaUpdate{Role: data.CanonicalSnapshotRole, Version: 0, Data: snapshotJSON}))

	_, gottenSnapshot, err := GetOrCreateSnapshot("gun", store, crypto)
	require.NoError(t, err, "GetSnapshot errored")

	require.False(t, bytes.Equal(snapshotJSON, gottenSnapshot),
		"Snapshot was not regenerated when old one was expired")

	signedMeta := &data.SignedMeta{}
	require.NoError(t, json.Unmarshal(gottenSnapshot, signedMeta))
	// the new metadata is not expired
	require.True(t, signedMeta.Signed.Expires.After(time.Now()))
}

// If the root is missing or corrupt, no snapshot can be generated
func TestCannotMakeNewSnapshotIfNoRoot(t *testing.T) {
	repo, crypto, err := testutils.EmptyRepo("gun")
	require.NoError(t, err)

	// create an expired snapshot
	_, err = repo.SignSnapshot(time.Now().AddDate(-1, -1, -1))
	require.True(t, repo.Snapshot.Signed.Expires.Before(time.Now()))
	require.NoError(t, err)
	snapshotJSON, err := json.Marshal(repo.Snapshot)
	require.NoError(t, err)

	for _, rootJSON := range [][]byte{nil, []byte("invalid JSON")} {
		store := storage.NewMemStorage()

		if rootJSON != nil {
			require.NoError(t, store.UpdateCurrent("gun",
				storage.MetaUpdate{Role: data.CanonicalRootRole, Version: 0, Data: rootJSON}))
		}
		require.NoError(t, store.UpdateCurrent("gun",
			storage.MetaUpdate{Role: data.CanonicalSnapshotRole, Version: 1, Data: snapshotJSON}))

		_, _, err := GetOrCreateSnapshot("gun", store, crypto)
		require.Error(t, err, "GetSnapshot errored")

		if rootJSON == nil { // missing metadata
			require.IsType(t, storage.ErrNotFound{}, err)
		} else {
			require.IsType(t, &json.SyntaxError{}, err)
		}
	}
}

func TestCreateSnapshotNoKeyInCrypto(t *testing.T) {
	store := storage.NewMemStorage()
	repo, _, err := testutils.EmptyRepo("gun")
	require.NoError(t, err)

	rootJSON, err := json.Marshal(repo.Root)
	require.NoError(t, err)

	// create an expired snapshot
	_, err = repo.SignSnapshot(time.Now().AddDate(-1, -1, -1))
	require.True(t, repo.Snapshot.Signed.Expires.Before(time.Now()))
	require.NoError(t, err)
	snapshotJSON, err := json.Marshal(repo.Snapshot)
	require.NoError(t, err)

	// set all the metadata so we know the failure to sign is just because of the key
	require.NoError(t, store.UpdateCurrent("gun",
		storage.MetaUpdate{Role: data.CanonicalRootRole, Version: 0, Data: rootJSON}))
	require.NoError(t, store.UpdateCurrent("gun",
		storage.MetaUpdate{Role: data.CanonicalSnapshotRole, Version: 0, Data: snapshotJSON}))

	// pass it a new cryptoservice without the key
	_, _, err = GetOrCreateSnapshot("gun", store, signed.NewEd25519())
	require.Error(t, err)
	require.IsType(t, signed.ErrNoKeys{}, err)
}

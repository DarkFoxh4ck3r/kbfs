// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"time"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/kbfs/kbfscodec"
	"github.com/keybase/kbfs/kbfscrypto"
	metrics "github.com/rcrowley/go-metrics"
	"golang.org/x/net/context"
)

// Block just needs to be (de)serialized using msgpack
type Block interface {
	// GetEncodedSize returns the encoded size of this block, but only
	// if it has been previously set; otherwise it returns 0.
	GetEncodedSize() uint32
	// SetEncodedSize sets the encoded size of this block, locally
	// caching it.  The encoded size is not serialized.
	SetEncodedSize(size uint32)
	// DataVersion returns the data version for this block
	DataVersion() DataVer
}

// NodeID is a unique but transient ID for a Node. That is, two Node
// objects in memory at the same time represent the same file or
// directory if and only if their NodeIDs are equal (by pointer).
type NodeID interface {
	// ParentID returns the NodeID of the directory containing the
	// pointed-to file or directory, or nil if none exists.
	ParentID() NodeID
}

// Node represents a direct pointer to a file or directory in KBFS.
// It is somewhat like an inode in a regular file system.  Users of
// KBFS can use Node as a handle when accessing files or directories
// they have previously looked up.
type Node interface {
	// GetID returns the ID of this Node. This should be used as a
	// map key instead of the Node itself.
	GetID() NodeID
	// GetFolderBranch returns the folder ID and branch for this Node.
	GetFolderBranch() FolderBranch
	// GetBasename returns the current basename of the node, or ""
	// if the node has been unlinked.
	GetBasename() string
}

// KBFSOps handles all file system operations.  Expands all indirect
// pointers.  Operations that modify the server data change all the
// block IDs along the path, and so must return a path with the new
// BlockIds so the caller can update their references.
//
// KBFSOps implementations must guarantee goroutine-safety of calls on
// a per-top-level-folder basis.
//
// There are two types of operations that could block:
//   * remote-sync operations, that need to synchronously update the
//     MD for the corresponding top-level folder.  When these
//     operations return successfully, they will have guaranteed to
//     have successfully written the modification to the KBFS servers.
//   * remote-access operations, that don't sync any modifications to KBFS
//     servers, but may block on reading data from the servers.
//
// KBFSOps implementations are supposed to give git-like consistency
// semantics for modification operations; they will be visible to
// other clients immediately after the remote-sync operations succeed,
// if and only if there was no other intervening modification to the
// same folder.  If not, the change will be sync'd to the server in a
// special per-device "unmerged" area before the operation succeeds.
// In this case, the modification will not be visible to other clients
// until the KBFS code on this device performs automatic conflict
// resolution in the background.
//
// All methods take a Context (see https://blog.golang.org/context),
// and if that context is cancelled during the operation, KBFSOps will
// abort any blocking calls and return ctx.Err(). Any notifications
// resulting from an operation will also include this ctx (or a
// Context derived from it), allowing the caller to determine whether
// the notification is a result of their own action or an external
// action.
type KBFSOps interface {
	// GetFavorites returns the logged-in user's list of favorite
	// top-level folders.  This is a remote-access operation.
	GetFavorites(ctx context.Context) ([]Favorite, error)
	// RefreshCachedFavorites tells the instances to forget any cached
	// favorites list and fetch a new list from the server.  The
	// effects are asychronous; if there's an error refreshing the
	// favorites, the cached favorites will become empty.
	RefreshCachedFavorites(ctx context.Context)
	// AddFavorite adds the favorite to both the server and
	// the local cache.
	AddFavorite(ctx context.Context, fav Favorite) error
	// DeleteFavorite deletes the favorite from both the server and
	// the local cache.  Idempotent, so it succeeds even if the folder
	// isn't favorited.
	DeleteFavorite(ctx context.Context, fav Favorite) error

	// GetTLFCryptKeys gets crypt key of all generations as well as
	// TLF ID for tlfHandle. The returned keys (the keys slice) are ordered by
	// generation, starting with the key for FirstValidKeyGen.
	GetTLFCryptKeys(ctx context.Context, tlfHandle *TlfHandle) (
		keys []kbfscrypto.TLFCryptKey, id TlfID, err error)

	// GetTLFID gets the TlfID for tlfHandle.
	GetTLFID(ctx context.Context, tlfHandle *TlfHandle) (TlfID, error)

	// GetOrCreateRootNode returns the root node and root entry
	// info associated with the given TLF handle and branch, if
	// the logged-in user has read permissions to the top-level
	// folder. It creates the folder if one doesn't exist yet (and
	// branch == MasterBranch), and the logged-in user has write
	// permissions to the top-level folder.  This is a
	// remote-access operation.
	GetOrCreateRootNode(
		ctx context.Context, h *TlfHandle, branch BranchName) (
		node Node, ei EntryInfo, err error)
	// GetRootNode is like GetOrCreateRootNode but if the root node
	// does not exist it will return a nil Node and not create it.
	GetRootNode(
		ctx context.Context, h *TlfHandle, branch BranchName) (
		node Node, ei EntryInfo, err error)
	// GetDirChildren returns a map of children in the directory,
	// mapped to their EntryInfo, if the logged-in user has read
	// permission for the top-level folder.  This is a remote-access
	// operation.
	GetDirChildren(ctx context.Context, dir Node) (map[string]EntryInfo, error)
	// Lookup returns the Node and entry info associated with a
	// given name in a directory, if the logged-in user has read
	// permissions to the top-level folder.  The returned Node is nil
	// if the name is a symlink.  This is a remote-access operation.
	Lookup(ctx context.Context, dir Node, name string) (Node, EntryInfo, error)
	// Stat returns the entry info associated with a
	// given Node, if the logged-in user has read permissions to the
	// top-level folder.  This is a remote-access operation.
	Stat(ctx context.Context, node Node) (EntryInfo, error)
	// CreateDir creates a new subdirectory under the given node, if
	// the logged-in user has write permission to the top-level
	// folder.  Returns the new Node for the created subdirectory, and
	// its new entry info.  This is a remote-sync operation.
	CreateDir(ctx context.Context, dir Node, name string) (
		Node, EntryInfo, error)
	// CreateFile creates a new file under the given node, if the
	// logged-in user has write permission to the top-level folder.
	// Returns the new Node for the created file, and its new
	// entry info. excl (when implemented) specifies whether this is an exclusive
	// create.  Semantically setting excl to WithExcl is like O_CREAT|O_EXCL in a
	// Unix open() call.
	//
	// This is a remote-sync operation.
	CreateFile(ctx context.Context, dir Node, name string, isExec bool, excl Excl) (
		Node, EntryInfo, error)
	// CreateLink creates a new symlink under the given node, if the
	// logged-in user has write permission to the top-level folder.
	// Returns the new entry info for the created symlink.  This
	// is a remote-sync operation.
	CreateLink(ctx context.Context, dir Node, fromName string, toPath string) (
		EntryInfo, error)
	// RemoveDir removes the subdirectory represented by the given
	// node, if the logged-in user has write permission to the
	// top-level folder.  Will return an error if the subdirectory is
	// not empty.  This is a remote-sync operation.
	RemoveDir(ctx context.Context, dir Node, dirName string) error
	// RemoveEntry removes the directory entry represented by the
	// given node, if the logged-in user has write permission to the
	// top-level folder.  This is a remote-sync operation.
	RemoveEntry(ctx context.Context, dir Node, name string) error
	// Rename performs an atomic rename operation with a given
	// top-level folder if the logged-in user has write permission to
	// that folder, and will return an error if nodes from different
	// folders are passed in.  Also returns an error if the new name
	// already has an entry corresponding to an existing directory
	// (only non-dir types may be renamed over).  This is a
	// remote-sync operation.
	Rename(ctx context.Context, oldParent Node, oldName string, newParent Node,
		newName string) error
	// Read fills in the given buffer with data from the file at the
	// given node starting at the given offset, if the logged-in user
	// has read permission to the top-level folder.  The read data
	// reflects any outstanding writes and truncates to that file that
	// have been written through this KBFSOps object, even if those
	// writes have not yet been sync'd.  There is no guarantee that
	// Read returns all of the requested data; it will return the
	// number of bytes that it wrote to the dest buffer.  Reads on an
	// unlinked file may or may not succeed, depending on whether or
	// not the data has been cached locally.  If (0, nil) is returned,
	// that means EOF has been reached. This is a remote-access
	// operation.
	Read(ctx context.Context, file Node, dest []byte, off int64) (int64, error)
	// Write modifies the file at the given node, by writing the given
	// buffer at the given offset within the file, if the logged-in
	// user has write permission to the top-level folder.  It
	// overwrites any data already there, and extends the file size as
	// necessary to accomodate the new data.  It guarantees to write
	// the entire buffer in one operation.  Writes on an unlinked file
	// may or may not succeed as no-ops, depending on whether or not
	// the necessary blocks have been locally cached.  This is a
	// remote-access operation.
	Write(ctx context.Context, file Node, data []byte, off int64) error
	// Truncate modifies the file at the given node, by either
	// shrinking or extending its size to match the given size, if the
	// logged-in user has write permission to the top-level folder.
	// If extending the file, it pads the new data with 0s.  Truncates
	// on an unlinked file may or may not succeed as no-ops, depending
	// on whether or not the necessary blocks have been locally
	// cached.  This is a remote-access operation.
	Truncate(ctx context.Context, file Node, size uint64) error
	// SetEx turns on or off the executable bit on the file
	// represented by a given node, if the logged-in user has write
	// permissions to the top-level folder.  This is a remote-sync
	// operation.
	SetEx(ctx context.Context, file Node, ex bool) error
	// SetMtime sets the modification time on the file represented by
	// a given node, if the logged-in user has write permissions to
	// the top-level folder.  If mtime is nil, it is a noop.  This is
	// a remote-sync operation.
	SetMtime(ctx context.Context, file Node, mtime *time.Time) error
	// Sync flushes all outstanding writes and truncates for the given
	// file to the KBFS servers, if the logged-in user has write
	// permissions to the top-level folder.  If done through a file
	// system interface, this may include modifications done via
	// multiple file handles.  This is a remote-sync operation.
	Sync(ctx context.Context, file Node) error
	// FolderStatus returns the status of a particular folder/branch, along
	// with a channel that will be closed when the status has been
	// updated (to eliminate the need for polling this method).
	FolderStatus(ctx context.Context, folderBranch FolderBranch) (
		FolderBranchStatus, <-chan StatusUpdate, error)
	// Status returns the status of KBFS, along with a channel that will be
	// closed when the status has been updated (to eliminate the need for
	// polling this method). KBFSStatus can be non-empty even if there is an
	// error.
	Status(ctx context.Context) (
		KBFSStatus, <-chan StatusUpdate, error)
	// UnstageForTesting clears out this device's staged state, if
	// any, and fast-forwards to the current head of this
	// folder-branch.
	UnstageForTesting(ctx context.Context, folderBranch FolderBranch) error
	// Rekey rekeys this folder.
	Rekey(ctx context.Context, id TlfID) error
	// SyncFromServerForTesting blocks until the local client has
	// contacted the server and guaranteed that all known updates
	// for the given top-level folder have been applied locally
	// (and notifications sent out to any observers).  It returns
	// an error if this folder-branch is currently unmerged or
	// dirty locally.
	SyncFromServerForTesting(ctx context.Context, folderBranch FolderBranch) error
	// GetUpdateHistory returns a complete history of all the merged
	// updates of the given folder, in a data structure that's
	// suitable for encoding directly into JSON.  This is an expensive
	// operation, and should only be used for ocassional debugging.
	// Note that the history does not include any unmerged changes or
	// outstanding writes from the local device.
	GetUpdateHistory(ctx context.Context, folderBranch FolderBranch) (
		history TLFUpdateHistory, err error)
	// GetEditHistory returns a clustered list of the most recent file
	// edits by each of the valid writers of the given folder.  users
	// looking to get updates to this list can register as an observer
	// for the folder.
	GetEditHistory(ctx context.Context, folderBranch FolderBranch) (
		edits TlfWriterEdits, err error)

	// GetNodeMetadata gets metadata associated with a Node.
	GetNodeMetadata(ctx context.Context, node Node) (NodeMetadata, error)

	// Shutdown is called to clean up any resources associated with
	// this KBFSOps instance.
	Shutdown() error
	// PushConnectionStatusChange updates the status of a service for
	// human readable connection status tracking.
	PushConnectionStatusChange(service string, newStatus error)
}

// KeybaseService is an interface for communicating with the keybase
// service.
type KeybaseService interface {
	// Resolve, given an assertion, resolves it to a username/UID
	// pair. The username <-> UID mapping is trusted and
	// immutable, so it can be cached. If the assertion is just
	// the username or a UID assertion, then the resolution can
	// also be trusted. If the returned pair is equal to that of
	// the current session, then it can also be
	// trusted. Otherwise, Identify() needs to be called on the
	// assertion before the assertion -> (username, UID) mapping
	// can be trusted.
	Resolve(ctx context.Context, assertion string) (
		libkb.NormalizedUsername, keybase1.UID, error)

	// Identify, given an assertion, returns a UserInfo struct
	// with the user that matches that assertion, or an error
	// otherwise. The reason string is displayed on any tracker
	// popups spawned.
	Identify(ctx context.Context, assertion, reason string) (UserInfo, error)

	// LoadUserPlusKeys returns a UserInfo struct for a
	// user with the specified UID.
	// If you have the UID for a user and don't require Identify to
	// validate an assertion or the identity of a user, use this to
	// get UserInfo structs as it is much cheaper than Identify.
	LoadUserPlusKeys(ctx context.Context, uid keybase1.UID) (UserInfo, error)

	// LoadUnverifiedKeys returns a list of unverified public keys.  They are the union
	// of all known public keys associated with the account and the currently verified
	// keys currently part of the user's sigchain.
	LoadUnverifiedKeys(ctx context.Context, uid keybase1.UID) (
		[]keybase1.PublicKey, error)

	// CurrentSession returns a SessionInfo struct with all the
	// information for the current session, or an error otherwise.
	CurrentSession(ctx context.Context, sessionID int) (SessionInfo, error)

	// FavoriteAdd adds the given folder to the list of favorites.
	FavoriteAdd(ctx context.Context, folder keybase1.Folder) error

	// FavoriteAdd removes the given folder from the list of
	// favorites.
	FavoriteDelete(ctx context.Context, folder keybase1.Folder) error

	// FavoriteList returns the current list of favorites.
	FavoriteList(ctx context.Context, sessionID int) ([]keybase1.Folder, error)

	// Notify sends a filesystem notification.
	Notify(ctx context.Context, notification *keybase1.FSNotification) error

	// NotifySyncStatus sends a sync status notification.
	NotifySyncStatus(ctx context.Context,
		status *keybase1.FSPathSyncStatus) error

	// FlushUserFromLocalCache instructs this layer to clear any
	// KBFS-side, locally-cached information about the given user.
	// This does NOT involve communication with the daemon, this is
	// just to force future calls loading this user to fall through to
	// the daemon itself, rather than being served from the cache.
	FlushUserFromLocalCache(ctx context.Context, uid keybase1.UID)

	// FlushUserUnverifiedKeysFromLocalCache instructs this layer to clear any
	// KBFS-side, locally-cached unverified keys for the given user.
	FlushUserUnverifiedKeysFromLocalCache(ctx context.Context, uid keybase1.UID)

	// TODO: Add CryptoClient methods, too.

	// Shutdown frees any resources associated with this
	// instance. No other methods may be called after this is
	// called.
	Shutdown()
}

// KeybaseServiceCn defines methods needed to construct KeybaseService
// and Crypto implementations.
type KeybaseServiceCn interface {
	NewKeybaseService(config Config, params InitParams, ctx Context, log logger.Logger) (KeybaseService, error)
	NewCrypto(config Config, params InitParams, ctx Context, log logger.Logger) (Crypto, error)
}

type resolver interface {
	// Resolve, given an assertion, resolves it to a username/UID
	// pair. The username <-> UID mapping is trusted and
	// immutable, so it can be cached. If the assertion is just
	// the username or a UID assertion, then the resolution can
	// also be trusted. If the returned pair is equal to that of
	// the current session, then it can also be
	// trusted. Otherwise, Identify() needs to be called on the
	// assertion before the assertion -> (username, UID) mapping
	// can be trusted.
	Resolve(ctx context.Context, assertion string) (
		libkb.NormalizedUsername, keybase1.UID, error)
}

type identifier interface {
	// Identify resolves an assertion (which could also be a
	// username) to a UserInfo struct, spawning tracker popups if
	// necessary.  The reason string is displayed on any tracker
	// popups spawned.
	Identify(ctx context.Context, assertion, reason string) (UserInfo, error)
}

type normalizedUsernameGetter interface {
	// GetNormalizedUsername returns the normalized username
	// corresponding to the given UID.
	GetNormalizedUsername(ctx context.Context, uid keybase1.UID) (libkb.NormalizedUsername, error)
}

type currentInfoGetter interface {
	// GetCurrentToken gets the current keybase session token.
	GetCurrentToken(ctx context.Context) (string, error)
	// GetCurrentUserInfo gets the name and UID of the current
	// logged-in user.
	GetCurrentUserInfo(ctx context.Context) (
		libkb.NormalizedUsername, keybase1.UID, error)
	// GetCurrentCryptPublicKey gets the crypt public key for the
	// currently-active device.
	GetCurrentCryptPublicKey(ctx context.Context) (
		kbfscrypto.CryptPublicKey, error)
	// GetCurrentVerifyingKey gets the public key used for signing for the
	// currently-active device.
	GetCurrentVerifyingKey(ctx context.Context) (
		kbfscrypto.VerifyingKey, error)
}

// KBPKI interacts with the Keybase daemon to fetch user info.
type KBPKI interface {
	currentInfoGetter
	resolver
	identifier
	normalizedUsernameGetter

	// HasVerifyingKey returns nil if the given user has the given
	// VerifyingKey, and an error otherwise.
	HasVerifyingKey(ctx context.Context, uid keybase1.UID,
		verifyingKey kbfscrypto.VerifyingKey,
		atServerTime time.Time) error

	// HasUnverifiedVerifyingKey returns nil if the given user has the given
	// unverified VerifyingKey, and an error otherwise.  Note that any match
	// is with a key not verified to be currently connected to the user via
	// their sigchain.  This is currently only used to verify finalized or
	// reset TLFs.  Further note that unverified keys is a super set of
	// verified keys.
	HasUnverifiedVerifyingKey(ctx context.Context, uid keybase1.UID,
		verifyingKey kbfscrypto.VerifyingKey) error

	// GetCryptPublicKeys gets all of a user's crypt public keys (including
	// paper keys).
	GetCryptPublicKeys(ctx context.Context, uid keybase1.UID) (
		[]kbfscrypto.CryptPublicKey, error)

	// TODO: Split the methods below off into a separate
	// FavoriteOps interface.

	// FavoriteAdd adds folder to the list of the logged in user's
	// favorite folders.  It is idempotent.
	FavoriteAdd(ctx context.Context, folder keybase1.Folder) error

	// FavoriteDelete deletes folder from the list of the logged in user's
	// favorite folders.  It is idempotent.
	FavoriteDelete(ctx context.Context, folder keybase1.Folder) error

	// FavoriteList returns the list of all favorite folders for
	// the logged in user.
	FavoriteList(ctx context.Context) ([]keybase1.Folder, error)

	// Notify sends a filesystem notification.
	Notify(ctx context.Context, notification *keybase1.FSNotification) error
}

// KeyMetadata is an interface for something that holds key
// information. This is usually implemented by RootMetadata.
type KeyMetadata interface {
	// TlfID returns the ID of the TLF for which this object holds
	// key info.
	TlfID() TlfID

	// LatestKeyGeneration returns the most recent key generation
	// with key data in this object, or PublicKeyGen if this TLF
	// is public.
	LatestKeyGeneration() KeyGen

	// GetTlfHandle returns the handle for the TLF. It must not
	// return nil.
	//
	// TODO: Remove the need for this function in this interface,
	// so that BareRootMetadata can implement this interface
	// fully.
	GetTlfHandle() *TlfHandle

	// HasKeyForUser returns whether or not the given user has
	// keys for at least one device at the given key
	// generation. Returns false if the TLF is public, or if the
	// given key generation is invalid.
	HasKeyForUser(keyGen KeyGen, user keybase1.UID) bool

	// GetTLFCryptKeyParams returns all the necessary info to
	// construct the TLF crypt key for the given key generation,
	// user, and device (identified by its crypt public key), or
	// false if not found. This returns an error if the TLF is
	// public.
	GetTLFCryptKeyParams(
		keyGen KeyGen, user keybase1.UID,
		key kbfscrypto.CryptPublicKey) (
		kbfscrypto.TLFEphemeralPublicKey,
		EncryptedTLFCryptKeyClientHalf,
		TLFCryptKeyServerHalfID, bool, error)

	// StoresHistoricTLFCryptKeys returns whether or not history keys are
	// symmetrically encrypted; if not, they're encrypted per-device.
	StoresHistoricTLFCryptKeys() bool

	// GetHistoricTLFCryptKey attempts to symmetrically decrypt the key at the given
	// generation using the current generation's TLFCryptKey.
	GetHistoricTLFCryptKey(c cryptoPure, keyGen KeyGen,
		currentKey kbfscrypto.TLFCryptKey) (
		kbfscrypto.TLFCryptKey, error)
}

type encryptionKeyGetter interface {
	// GetTLFCryptKeyForEncryption gets the crypt key to use for
	// encryption (i.e., with the latest key generation) for the
	// TLF with the given metadata.
	GetTLFCryptKeyForEncryption(ctx context.Context, kmd KeyMetadata) (
		kbfscrypto.TLFCryptKey, error)
}

// KeyManager fetches and constructs the keys needed for KBFS file
// operations.
type KeyManager interface {
	encryptionKeyGetter

	// GetTLFCryptKeyForMDDecryption gets the crypt key to use for the
	// TLF with the given metadata to decrypt the private portion of
	// the metadata.  It finds the appropriate key from mdWithKeys
	// (which in most cases is the same as mdToDecrypt) if it's not
	// already cached.
	GetTLFCryptKeyForMDDecryption(ctx context.Context,
		kmdToDecrypt, kmdWithKeys KeyMetadata) (
		kbfscrypto.TLFCryptKey, error)

	// GetTLFCryptKeyForBlockDecryption gets the crypt key to use
	// for the TLF with the given metadata to decrypt the block
	// pointed to by the given pointer.
	GetTLFCryptKeyForBlockDecryption(ctx context.Context, kmd KeyMetadata,
		blockPtr BlockPointer) (kbfscrypto.TLFCryptKey, error)

	// GetTLFCryptKeyOfAllGenerations gets the crypt keys of all generations
	// for current devices. keys contains crypt keys from all generations, in
	// order, starting from FirstValidKeyGen.
	GetTLFCryptKeyOfAllGenerations(ctx context.Context, kmd KeyMetadata) (
		keys []kbfscrypto.TLFCryptKey, err error)

	// Rekey checks the given MD object, if it is a private TLF,
	// against the current set of device keys for all valid
	// readers and writers.  If there are any new devices, it
	// updates all existing key generations to include the new
	// devices.  If there are devices that have been removed, it
	// creates a new epoch of keys for the TLF.  If no devices
	// have changed, or if there was an error, it returns false.
	// Otherwise, it returns true. If a new key generation is
	// added the second return value points to this new key. This
	// is to allow for caching of the TLF crypt key only after a
	// successful merged write of the metadata. Otherwise we could
	// prematurely pollute the key cache.
	//
	// If the given MD object is a public TLF, it simply updates
	// the TLF's handle with any newly-resolved writers.
	//
	// If promptPaper is set, prompts for any unlocked paper keys.
	// promptPaper shouldn't be set if md is for a public TLF.
	Rekey(ctx context.Context, md *RootMetadata, promptPaper bool) (
		bool, *kbfscrypto.TLFCryptKey, error)
}

// Reporter exports events (asynchronously) to any number of sinks
type Reporter interface {
	// ReportErr records that a given error happened.
	ReportErr(ctx context.Context, tlfName CanonicalTlfName, public bool,
		mode ErrorModeType, err error)
	// AllKnownErrors returns all errors known to this Reporter.
	AllKnownErrors() []ReportedError
	// Notify sends the given notification to any sink.
	Notify(ctx context.Context, notification *keybase1.FSNotification)
	// NotifySyncStatus sends the given path sync status to any sink.
	NotifySyncStatus(ctx context.Context, status *keybase1.FSPathSyncStatus)
	// Shutdown frees any resources allocated by a Reporter.
	Shutdown()
}

// MDCache gets and puts plaintext top-level metadata into the cache.
type MDCache interface {
	// Get gets the metadata object associated with the given TlfID,
	// revision number, and branch ID (NullBranchID for merged MD).
	Get(tlf TlfID, rev MetadataRevision, bid BranchID) (ImmutableRootMetadata, error)
	// Put stores the metadata object.
	Put(md ImmutableRootMetadata) error
	// Delete removes the given metadata object from the cache if it exists.
	Delete(tlf TlfID, rev MetadataRevision, bid BranchID)
	// Replace replaces the entry matching the md under the old branch
	// ID with the new one.  If the old entry doesn't exist, this is
	// equivalent to a Put.
	Replace(newRmd ImmutableRootMetadata, oldBID BranchID) error
}

// KeyCache handles caching for both TLFCryptKeys and BlockCryptKeys.
type KeyCache interface {
	// GetTLFCryptKey gets the crypt key for the given TLF.
	GetTLFCryptKey(TlfID, KeyGen) (kbfscrypto.TLFCryptKey, error)
	// PutTLFCryptKey stores the crypt key for the given TLF.
	PutTLFCryptKey(TlfID, KeyGen, kbfscrypto.TLFCryptKey) error
}

// BlockCacheLifetime denotes the lifetime of an entry in BlockCache.
type BlockCacheLifetime int

const (
	// TransientEntry means that the cache entry may be evicted at
	// any time.
	TransientEntry BlockCacheLifetime = iota
	// PermanentEntry means that the cache entry must remain until
	// explicitly removed from the cache.
	PermanentEntry
)

// BlockCache gets and puts plaintext dir blocks and file blocks into
// a cache.  These blocks are immutable and identified by their
// content hash.
type BlockCache interface {
	// Get gets the block associated with the given block ID.
	Get(ptr BlockPointer) (Block, error)
	// CheckForKnownPtr sees whether this cache has a transient
	// entry for the given file block, which must be a direct file
	// block containing data).  Returns the full BlockPointer
	// associated with that ID, including key and data versions.
	// If no ID is known, return an uninitialized BlockPointer and
	// a nil error.
	CheckForKnownPtr(tlf TlfID, block *FileBlock) (BlockPointer, error)
	// Put stores the final (content-addressable) block associated
	// with the given block ID. If lifetime is TransientEntry,
	// then it is assumed that the block exists on the server and
	// the entry may be evicted from the cache at any time. If
	// lifetime is PermanentEntry, then it is assumed that the
	// block doesn't exist on the server and must remain in the
	// cache until explicitly removed. As an intermediary state,
	// as when a block is being sent to the server, the block may
	// be put into the cache both with TransientEntry and
	// PermanentEntry -- these are two separate entries. This is
	// fine, since the block should be the same.
	Put(ptr BlockPointer, tlf TlfID, block Block,
		lifetime BlockCacheLifetime) error
	// DeleteTransient removes the transient entry for the given
	// pointer from the cache, as well as any cached IDs so the block
	// won't be reused.
	DeleteTransient(ptr BlockPointer, tlf TlfID) error
	// Delete removes the permanent entry for the non-dirty block
	// associated with the given block ID from the cache.  No
	// error is returned if no block exists for the given ID.
	DeletePermanent(id BlockID) error
	// DeleteKnownPtr removes the cached ID for the given file
	// block. It does not remove the block itself.
	DeleteKnownPtr(tlf TlfID, block *FileBlock) error
}

// DirtyPermChan is a channel that gets closed when the holder has
// permission to write.  We are forced to define it as a type due to a
// bug in mockgen that can't handle return values with a chan
// struct{}.
type DirtyPermChan <-chan struct{}

// DirtyBlockCache gets and puts plaintext dir blocks and file blocks
// into a cache, which have been modified by the application and not
// yet committed on the KBFS servers.  They are identified by a
// (potentially random) ID that may not have any relationship with
// their context, along with a Branch in case the same TLF is being
// modified via multiple branches.  Dirty blocks are never evicted,
// they must be deleted explicitly.
type DirtyBlockCache interface {
	// Get gets the block associated with the given block ID.  Returns
	// the dirty block for the given ID, if one exists.
	Get(tlfID TlfID, ptr BlockPointer, branch BranchName) (Block, error)
	// Put stores a dirty block currently identified by the
	// given block pointer and branch name.
	Put(tlfID TlfID, ptr BlockPointer, branch BranchName, block Block) error
	// Delete removes the dirty block associated with the given block
	// pointer and branch from the cache.  No error is returned if no
	// block exists for the given ID.
	Delete(tlfID TlfID, ptr BlockPointer, branch BranchName) error
	// IsDirty states whether or not the block associated with the
	// given block pointer and branch name is dirty in this cache.
	IsDirty(tlfID TlfID, ptr BlockPointer, branch BranchName) bool
	// IsAnyDirty returns whether there are any dirty blocks in the
	// cache. tlfID may be ignored.
	IsAnyDirty(tlfID TlfID) bool
	// RequestPermissionToDirty is called whenever a user wants to
	// write data to a file.  The caller provides an estimated number
	// of bytes that will become dirty -- this is difficult to know
	// exactly without pre-fetching all the blocks involved, but in
	// practice we can just use the number of bytes sent in via the
	// Write. It returns a channel that blocks until the cache is
	// ready to receive more dirty data, at which point the channel is
	// closed.  The user must call
	// `UpdateUnsyncedBytes(-estimatedDirtyBytes)` once it has
	// completed its write and called `UpdateUnsyncedBytes` for all
	// the exact dirty block sizes.
	RequestPermissionToDirty(ctx context.Context, tlfID TlfID,
		estimatedDirtyBytes int64) (DirtyPermChan, error)
	// UpdateUnsyncedBytes is called by a user, who has already been
	// granted permission to write, with the delta in block sizes that
	// were dirtied as part of the write.  So for example, if a
	// newly-dirtied block of 20 bytes was extended by 5 bytes, they
	// should send 25.  If on the next write (before any syncs), bytes
	// 10-15 of that same block were overwritten, they should send 0
	// over the channel because there were no new bytes.  If an
	// already-dirtied block is truncated, or if previously requested
	// bytes have now been updated more accurately in previous
	// requests, newUnsyncedBytes may be negative.  wasSyncing should
	// be true if `BlockSyncStarted` has already been called for this
	// block.
	UpdateUnsyncedBytes(tlfID TlfID, newUnsyncedBytes int64, wasSyncing bool)
	// UpdateSyncingBytes is called when a particular block has
	// started syncing, or with a negative number when a block is no
	// longer syncing due to an error (and BlockSyncFinished will
	// never be called).
	UpdateSyncingBytes(tlfID TlfID, size int64)
	// BlockSyncFinished is called when a particular block has
	// finished syncing, though the overall sync might not yet be
	// complete.  This lets the cache know it might be able to grant
	// more permission to writers.
	BlockSyncFinished(tlfID TlfID, size int64)
	// SyncFinished is called when a complete sync has completed and
	// its dirty blocks have been removed from the cache.  This lets
	// the cache know it might be able to grant more permission to
	// writers.
	SyncFinished(tlfID TlfID, size int64)
	// ShouldForceSync returns true if the sync buffer is full enough
	// to force all callers to sync their data immediately.
	ShouldForceSync(tlfID TlfID) bool

	// Shutdown frees any resources associated with this instance.  It
	// returns an error if there are any unsynced blocks.
	Shutdown() error
}

// cryptoPure contains all methods of Crypto that don't depend on
// implicit state, i.e. they're pure functions of the input.
type cryptoPure interface {
	// MakeRandomTlfID generates a dir ID using a CSPRNG.
	MakeRandomTlfID(isPublic bool) (TlfID, error)

	// MakeRandomBranchID generates a per-device branch ID using a CSPRNG.
	MakeRandomBranchID() (BranchID, error)

	// MakeMdID computes the MD ID of a RootMetadata object.
	// TODO: This should move to BareRootMetadata. Note though, that some mock tests
	// rely on it being part of the config and crypto_measured.go uses it to keep
	// statistics on time spent hashing.
	MakeMdID(md BareRootMetadata) (MdID, error)

	// MakeMerkleHash computes the hash of a RootMetadataSigned object
	// for inclusion into the KBFS Merkle tree.
	MakeMerkleHash(md *RootMetadataSigned) (MerkleHash, error)

	// MakeTemporaryBlockID generates a temporary block ID using a
	// CSPRNG. This is used for indirect blocks before they're
	// committed to the server.
	MakeTemporaryBlockID() (BlockID, error)

	// MakePermanentBlockID computes the permanent ID of a block
	// given its encoded and encrypted contents.
	MakePermanentBlockID(encodedEncryptedData []byte) (BlockID, error)

	// VerifyBlockID verifies that the given block ID is the
	// permanent block ID for the given encoded and encrypted
	// data.
	VerifyBlockID(encodedEncryptedData []byte, id BlockID) error

	// MakeRefNonce generates a block reference nonce using a
	// CSPRNG. This is used for distinguishing different references to
	// the same BlockID.
	MakeBlockRefNonce() (BlockRefNonce, error)

	// MakeRandomTLFKeys generates top-level folder keys using a CSPRNG.
	MakeRandomTLFKeys() (kbfscrypto.TLFPublicKey,
		kbfscrypto.TLFPrivateKey, kbfscrypto.TLFEphemeralPublicKey,
		kbfscrypto.TLFEphemeralPrivateKey, kbfscrypto.TLFCryptKey,
		error)
	// MakeRandomTLFCryptKeyServerHalf generates the server-side of a
	// top-level folder crypt key.
	MakeRandomTLFCryptKeyServerHalf() (
		kbfscrypto.TLFCryptKeyServerHalf, error)
	// MakeRandomBlockCryptKeyServerHalf generates the server-side of
	// a block crypt key.
	MakeRandomBlockCryptKeyServerHalf() (
		kbfscrypto.BlockCryptKeyServerHalf, error)

	// MaskTLFCryptKey returns the client-side of a top-level folder crypt key.
	MaskTLFCryptKey(serverHalf kbfscrypto.TLFCryptKeyServerHalf,
		key kbfscrypto.TLFCryptKey) (
		kbfscrypto.TLFCryptKeyClientHalf, error)
	// UnmaskTLFCryptKey returns the top-level folder crypt key.
	UnmaskTLFCryptKey(serverHalf kbfscrypto.TLFCryptKeyServerHalf,
		clientHalf kbfscrypto.TLFCryptKeyClientHalf) (
		kbfscrypto.TLFCryptKey, error)
	// UnmaskBlockCryptKey returns the block crypt key.
	UnmaskBlockCryptKey(serverHalf kbfscrypto.BlockCryptKeyServerHalf,
		tlfCryptKey kbfscrypto.TLFCryptKey) (
		kbfscrypto.BlockCryptKey, error)

	// Verify verifies that sig matches msg being signed with the
	// private key that corresponds to verifyingKey.
	Verify(msg []byte, sigInfo kbfscrypto.SignatureInfo) error

	// EncryptTLFCryptKeyClientHalf encrypts a TLFCryptKeyClientHalf
	// using both a TLF's ephemeral private key and a device pubkey.
	EncryptTLFCryptKeyClientHalf(
		privateKey kbfscrypto.TLFEphemeralPrivateKey,
		publicKey kbfscrypto.CryptPublicKey,
		clientHalf kbfscrypto.TLFCryptKeyClientHalf) (
		EncryptedTLFCryptKeyClientHalf, error)

	// EncryptPrivateMetadata encrypts a PrivateMetadata object.
	EncryptPrivateMetadata(
		pmd *PrivateMetadata, key kbfscrypto.TLFCryptKey) (
		EncryptedPrivateMetadata, error)
	// DecryptPrivateMetadata decrypts a PrivateMetadata object.
	DecryptPrivateMetadata(
		encryptedPMD EncryptedPrivateMetadata,
		key kbfscrypto.TLFCryptKey) (*PrivateMetadata, error)

	// EncryptBlocks encrypts a block. plainSize is the size of the encoded
	// block; EncryptBlock() must guarantee that plainSize <=
	// len(encryptedBlock).
	EncryptBlock(block Block, key kbfscrypto.BlockCryptKey) (
		plainSize int, encryptedBlock EncryptedBlock, err error)

	// DecryptBlock decrypts a block. Similar to EncryptBlock(),
	// DecryptBlock() must guarantee that (size of the decrypted
	// block) <= len(encryptedBlock).
	DecryptBlock(encryptedBlock EncryptedBlock,
		key kbfscrypto.BlockCryptKey, block Block) error

	// GetTLFCryptKeyServerHalfID creates a unique ID for this particular
	// kbfscrypto.TLFCryptKeyServerHalf.
	GetTLFCryptKeyServerHalfID(
		user keybase1.UID, deviceKID keybase1.KID,
		serverHalf kbfscrypto.TLFCryptKeyServerHalf) (
		TLFCryptKeyServerHalfID, error)

	// VerifyTLFCryptKeyServerHalfID verifies the ID is the proper HMAC result.
	VerifyTLFCryptKeyServerHalfID(serverHalfID TLFCryptKeyServerHalfID,
		user keybase1.UID, deviceKID keybase1.KID,
		serverHalf kbfscrypto.TLFCryptKeyServerHalf) error

	// EncryptMerkleLeaf encrypts a Merkle leaf node with the TLFPublicKey.
	EncryptMerkleLeaf(leaf MerkleLeaf, pubKey kbfscrypto.TLFPublicKey,
		nonce *[24]byte, ePrivKey kbfscrypto.TLFEphemeralPrivateKey) (
		EncryptedMerkleLeaf, error)

	// DecryptMerkleLeaf decrypts a Merkle leaf node with the TLFPrivateKey.
	DecryptMerkleLeaf(encryptedLeaf EncryptedMerkleLeaf,
		privKey kbfscrypto.TLFPrivateKey, nonce *[24]byte,
		ePubKey kbfscrypto.TLFEphemeralPublicKey) (*MerkleLeaf, error)

	// MakeTLFWriterKeyBundleID hashes a TLFWriterKeyBundleV3 to create an ID.
	MakeTLFWriterKeyBundleID(wkb *TLFWriterKeyBundleV3) (TLFWriterKeyBundleID, error)

	// MakeTLFReaderKeyBundleID hashes a TLFReaderKeyBundleV3 to create an ID.
	MakeTLFReaderKeyBundleID(rkb *TLFReaderKeyBundleV3) (TLFReaderKeyBundleID, error)

	// EncryptTLFCryptKeys encrypts an array of historic TLFCryptKeys.
	EncryptTLFCryptKeys(oldKeys []kbfscrypto.TLFCryptKey,
		key kbfscrypto.TLFCryptKey) (
		EncryptedTLFCryptKeys, error)

	// DecryptTLFCryptKeys decrypts an array of historic TLFCryptKeys.
	DecryptTLFCryptKeys(
		encKeys EncryptedTLFCryptKeys, key kbfscrypto.TLFCryptKey) (
		[]kbfscrypto.TLFCryptKey, error)
}

// Duplicate kbfscrypto.Signer here to work around gomock's
// limitations.
type cryptoSigner interface {
	Sign(context.Context, []byte) (kbfscrypto.SignatureInfo, error)
	SignToString(context.Context, []byte) (string, error)
}

// Crypto signs, verifies, encrypts, and decrypts stuff.
type Crypto interface {
	cryptoPure
	cryptoSigner

	// DecryptTLFCryptKeyClientHalf decrypts a
	// kbfscrypto.TLFCryptKeyClientHalf using the current device's
	// private key and the TLF's ephemeral public key.
	DecryptTLFCryptKeyClientHalf(ctx context.Context,
		publicKey kbfscrypto.TLFEphemeralPublicKey,
		encryptedClientHalf EncryptedTLFCryptKeyClientHalf) (
		kbfscrypto.TLFCryptKeyClientHalf, error)

	// DecryptTLFCryptKeyClientHalfAny decrypts one of the
	// kbfscrypto.TLFCryptKeyClientHalf using the available
	// private keys and the ephemeral public key.  If promptPaper
	// is true, the service will prompt the user for any unlocked
	// paper keys.
	DecryptTLFCryptKeyClientHalfAny(ctx context.Context,
		keys []EncryptedTLFCryptKeyClientAndEphemeral,
		promptPaper bool) (
		kbfscrypto.TLFCryptKeyClientHalf, int, error)

	// Shutdown frees any resources associated with this instance.
	Shutdown()
}

// MDOps gets and puts root metadata to an MDServer.  On a get, it
// verifies the metadata is signed by the metadata's signing key.
type MDOps interface {
	// GetForHandle returns the current metadata object
	// corresponding to the given top-level folder's handle and
	// merge status, if the logged-in user has read permission on
	// the folder.  It creates the folder if one doesn't exist
	// yet, and the logged-in user has permission to do so.
	GetForHandle(
		ctx context.Context, handle *TlfHandle, mStatus MergeStatus) (
		TlfID, ImmutableRootMetadata, error)

	// GetForTLF returns the current metadata object
	// corresponding to the given top-level folder, if the logged-in
	// user has read permission on the folder.
	GetForTLF(ctx context.Context, id TlfID) (ImmutableRootMetadata, error)

	// GetUnmergedForTLF is the same as the above but for unmerged
	// metadata.
	GetUnmergedForTLF(ctx context.Context, id TlfID, bid BranchID) (
		ImmutableRootMetadata, error)

	// GetRange returns a range of metadata objects corresponding to
	// the passed revision numbers (inclusive).
	GetRange(ctx context.Context, id TlfID, start, stop MetadataRevision) (
		[]ImmutableRootMetadata, error)

	// GetUnmergedRange is the same as the above but for unmerged
	// metadata history (inclusive).
	GetUnmergedRange(ctx context.Context, id TlfID, bid BranchID,
		start, stop MetadataRevision) ([]ImmutableRootMetadata, error)

	// Put stores the metadata object for the given
	// top-level folder.
	Put(ctx context.Context, rmd *RootMetadata) (MdID, error)

	// PutUnmerged is the same as the above but for unmerged
	// metadata history.
	PutUnmerged(ctx context.Context, rmd *RootMetadata) (MdID, error)

	// PruneBranch prunes all unmerged history for the given TLF
	// branch.
	PruneBranch(ctx context.Context, id TlfID, bid BranchID) error

	// GetLatestHandleForTLF returns the server's idea of the latest handle for the TLF,
	// which may not yet be reflected in the MD if the TLF hasn't been rekeyed since it
	// entered into a conflicting state.
	GetLatestHandleForTLF(ctx context.Context, id TlfID) (
		BareTlfHandle, error)
}

// KeyOps fetches server-side key halves from the key server.
type KeyOps interface {
	// GetTLFCryptKeyServerHalf gets a server-side key half for a
	// device given the key half ID.
	GetTLFCryptKeyServerHalf(ctx context.Context,
		serverHalfID TLFCryptKeyServerHalfID,
		cryptPublicKey kbfscrypto.CryptPublicKey) (
		kbfscrypto.TLFCryptKeyServerHalf, error)

	// PutTLFCryptKeyServerHalves stores a server-side key halves for a
	// set of users and devices.
	PutTLFCryptKeyServerHalves(ctx context.Context,
		serverKeyHalves map[keybase1.UID]map[keybase1.KID]kbfscrypto.TLFCryptKeyServerHalf) error

	// DeleteTLFCryptKeyServerHalf deletes a server-side key half for a
	// device given the key half ID.
	DeleteTLFCryptKeyServerHalf(ctx context.Context,
		uid keybase1.UID, kid keybase1.KID,
		serverHalfID TLFCryptKeyServerHalfID) error
}

// BlockOps gets and puts data blocks to a BlockServer. It performs
// the necessary crypto operations on each block.
type BlockOps interface {
	// Get gets the block associated with the given block pointer
	// (which belongs to the TLF with the given key metadata),
	// decrypts it if necessary, and fills in the provided block
	// object with its contents, if the logged-in user has read
	// permission for that block.
	Get(ctx context.Context, kmd KeyMetadata, blockPtr BlockPointer,
		block Block) error

	// Ready turns the given block (which belongs to the TLF with
	// the given key metadata) into encoded (and encrypted) data,
	// and calculates its ID and size, so that we can do a bunch
	// of block puts in parallel for every write. Ready() must
	// guarantee that plainSize <= readyBlockData.QuotaSize().
	Ready(ctx context.Context, kmd KeyMetadata, block Block) (
		id BlockID, plainSize int, readyBlockData ReadyBlockData, err error)

	// Delete instructs the server to delete the given block references.
	// It returns the number of not-yet deleted references to
	// each block reference
	Delete(ctx context.Context, tlfID TlfID, ptrs []BlockPointer) (
		liveCounts map[BlockID]int, err error)

	// Archive instructs the server to mark the given block references
	// as "archived"; that is, they are not being used in the current
	// view of the folder, and shouldn't be served to anyone other
	// than folder writers.
	Archive(ctx context.Context, tlfID TlfID, ptrs []BlockPointer) error
}

// Duplicate kbfscrypto.AuthTokenRefreshHandler here to work around
// gomock's limitations.
type authTokenRefreshHandler interface {
	RefreshAuthToken(context.Context)
}

// MDServer gets and puts metadata for each top-level directory.  The
// instantiation should be able to fetch session/user details via KBPKI.  On a
// put, the server is responsible for 1) ensuring the user has appropriate
// permissions for whatever modifications were made; 2) ensuring that
// LastModifyingWriter and LastModifyingUser are updated appropriately; and 3)
// detecting conflicting writes based on the previous root block ID (i.e., when
// it supports strict consistency).  On a get, it verifies the logged-in user
// has read permissions.
//
// TODO: Add interface for searching by time
type MDServer interface {
	authTokenRefreshHandler

	// GetForHandle returns the current (signed/encrypted) metadata
	// object corresponding to the given top-level folder's handle, if
	// the logged-in user has read permission on the folder.  It
	// creates the folder if one doesn't exist yet, and the logged-in
	// user has permission to do so.
	GetForHandle(ctx context.Context, handle BareTlfHandle,
		mStatus MergeStatus) (TlfID, *RootMetadataSigned, error)

	// GetForTLF returns the current (signed/encrypted) metadata object
	// corresponding to the given top-level folder, if the logged-in
	// user has read permission on the folder.
	GetForTLF(ctx context.Context, id TlfID, bid BranchID, mStatus MergeStatus) (
		*RootMetadataSigned, error)

	// GetRange returns a range of (signed/encrypted) metadata objects
	// corresponding to the passed revision numbers (inclusive).
	GetRange(ctx context.Context, id TlfID, bid BranchID, mStatus MergeStatus,
		start, stop MetadataRevision) ([]*RootMetadataSigned, error)

	// Put stores the (signed/encrypted) metadata object for the given
	// top-level folder. Note: If the unmerged bit is set in the metadata
	// block's flags bitmask it will be appended to the unmerged per-device
	// history.
	Put(ctx context.Context, rmds *RootMetadataSigned, extra ExtraMetadata) error

	// PruneBranch prunes all unmerged history for the given TLF branch.
	PruneBranch(ctx context.Context, id TlfID, bid BranchID) error

	// RegisterForUpdate tells the MD server to inform the caller when
	// there is a merged update with a revision number greater than
	// currHead, which did NOT originate from this same MD server
	// session.  This method returns a chan which can receive only a
	// single error before it's closed.  If the received err is nil,
	// then there is updated MD ready to fetch which didn't originate
	// locally; if it is non-nil, then the previous registration
	// cannot send the next notification (e.g., the connection to the
	// MD server may have failed). In either case, the caller must
	// re-register to get a new chan that can receive future update
	// notifications.
	RegisterForUpdate(ctx context.Context, id TlfID,
		currHead MetadataRevision) (<-chan error, error)

	// CheckForRekeys initiates the rekey checking process on the
	// server.  The server is allowed to delay this request, and so it
	// returns a channel for returning the error. Actual rekey
	// requests are expected to come in asynchronously.
	CheckForRekeys(ctx context.Context) <-chan error

	// TruncateLock attempts to take the history truncation lock for
	// this folder, for a TTL defined by the server.  Returns true if
	// the lock was successfully taken.
	TruncateLock(ctx context.Context, id TlfID) (bool, error)
	// TruncateUnlock attempts to release the history truncation lock
	// for this folder.  Returns true if the lock was successfully
	// released.
	TruncateUnlock(ctx context.Context, id TlfID) (bool, error)

	// DisableRekeyUpdatesForTesting disables processing rekey updates
	// received from the mdserver while testing.
	DisableRekeyUpdatesForTesting()

	// Shutdown is called to shutdown an MDServer connection.
	Shutdown()

	// IsConnected returns whether the MDServer is connected.
	IsConnected() bool

	// GetLatestHandleForTLF returns the server's idea of the latest handle for the TLF,
	// which may not yet be reflected in the MD if the TLF hasn't been rekeyed since it
	// entered into a conflicting state.  For the highest level of confidence, the caller
	// should verify the mapping with a Merkle tree lookup.
	GetLatestHandleForTLF(ctx context.Context, id TlfID) (
		BareTlfHandle, error)

	// OffsetFromServerTime is the current estimate for how off our
	// local clock is from the mdserver clock.  Add this to any
	// mdserver-provided timestamps to get the "local" time of the
	// corresponding event.  If the returned bool is false, then we
	// don't have a current estimate for the offset.
	OffsetFromServerTime() (time.Duration, bool)

	// GetKeyBundles returns the key bundles for the given key bundle IDs.
	GetKeyBundles(ctx context.Context,
		wkbID TLFWriterKeyBundleID,
		rkbID TLFReaderKeyBundleID) (
		*TLFWriterKeyBundleV3, *TLFReaderKeyBundleV3, error)
}

type mdServerLocal interface {
	MDServer
	addNewAssertionForTest(
		uid keybase1.UID, newAssertion keybase1.SocialAssertion) error
	getCurrentMergedHeadRevision(ctx context.Context, id TlfID) (
		rev MetadataRevision, err error)
	isShutdown() bool
	copy(config mdServerLocalConfig) mdServerLocal
}

// BlockServer gets and puts opaque data blocks.  The instantiation
// should be able to fetch session/user details via KBPKI.  On a
// put/delete, the server is reponsible for: 1) checking that the ID
// matches the hash of the buffer; and 2) enforcing writer quotas.
type BlockServer interface {
	authTokenRefreshHandler

	// Get gets the (encrypted) block data associated with the given
	// block ID and context, uses the provided block key to decrypt
	// the block, and fills in the provided block object with its
	// contents, if the logged-in user has read permission for that
	// block.
	Get(ctx context.Context, tlfID TlfID, id BlockID, context BlockContext) (
		[]byte, kbfscrypto.BlockCryptKeyServerHalf, error)
	// Put stores the (encrypted) block data under the given ID and
	// context on the server, along with the server half of the block
	// key.  context should contain a BlockRefNonce of zero.  There
	// will be an initial reference for this block for the given
	// context.
	//
	// Put should be idempotent, although it should also return an
	// error if, for a given ID, any of the other arguments differ
	// from previous Put calls with the same ID.
	//
	// If this returns a BServerErrorOverQuota, with Throttled=false,
	// the caller can treat it as informational and otherwise ignore
	// the error.
	Put(ctx context.Context, tlfID TlfID, id BlockID, context BlockContext,
		buf []byte, serverHalf kbfscrypto.BlockCryptKeyServerHalf) error

	// AddBlockReference adds a new reference to the given block,
	// defined by the given context (which should contain a non-zero
	// BlockRefNonce).  (Contexts with a BlockRefNonce of zero should
	// be used when putting the block for the first time via Put().)
	// Returns a BServerErrorBlockNonExistent if id is unknown within
	// this folder.
	//
	// AddBlockReference should be idempotent, although it should
	// also return an error if, for a given ID and refnonce, any
	// of the other fields of context differ from previous
	// AddBlockReference calls with the same ID and refnonce.
	//
	// If this returns a BServerErrorOverQuota, with Throttled=false,
	// the caller can treat it as informational and otherwise ignore
	// the error.
	AddBlockReference(ctx context.Context, tlfID TlfID, id BlockID,
		context BlockContext) error
	// RemoveBlockReferences removes the references to the given block
	// ID defined by the given contexts.  If no references to the block
	// remain after this call, the server is allowed to delete the
	// corresponding block permanently.  If the reference defined by
	// the count has already been removed, the call is a no-op.
	// It returns the number of remaining not-yet-deleted references after this
	// reference has been removed
	RemoveBlockReferences(ctx context.Context, tlfID TlfID,
		contexts map[BlockID][]BlockContext) (liveCounts map[BlockID]int, err error)

	// ArchiveBlockReferences marks the given block references as
	// "archived"; that is, they are not being used in the current
	// view of the folder, and shouldn't be served to anyone other
	// than folder writers.
	//
	// For a given ID/refnonce pair, ArchiveBlockReferences should
	// be idempotent, although it should also return an error if
	// any of the other fields of the context differ from previous
	// calls with the same ID/refnonce pair.
	ArchiveBlockReferences(ctx context.Context, tlfID TlfID,
		contexts map[BlockID][]BlockContext) error

	// Shutdown is called to shutdown a BlockServer connection.
	Shutdown()

	// GetUserQuotaInfo returns the quota for the user.
	GetUserQuotaInfo(ctx context.Context) (info *UserQuotaInfo, err error)
}

type blockRefLocalStatus int

const (
	liveBlockRef     blockRefLocalStatus = 1
	archivedBlockRef                     = 2
)

// blockServerLocal is the interface for BlockServer implementations
// that store data locally.
type blockServerLocal interface {
	BlockServer
	// getAll returns all the known block references, and should only be
	// used during testing.
	getAll(ctx context.Context, tlfID TlfID) (
		map[BlockID]map[BlockRefNonce]blockRefLocalStatus, error)
}

// BlockSplitter decides when a file or directory block needs to be split
type BlockSplitter interface {
	// CopyUntilSplit copies data into the block until we reach the
	// point where we should split, but only if writing to the end of
	// the last block.  If this is writing into the middle of a file,
	// just copy everything that will fit into the block, and assume
	// that block boundaries will be fixed later. Return how much was
	// copied.
	CopyUntilSplit(
		block *FileBlock, lastBlock bool, data []byte, off int64) int64

	// CheckSplit, given a block, figures out whether it ends at the
	// right place.  If so, return 0.  If not, return either the
	// offset in the block where it should be split, or -1 if more
	// bytes from the next block should be appended.
	CheckSplit(block *FileBlock) int64

	// ShouldEmbedBlockChanges decides whether we should keep the
	// block changes embedded in the MD or not.
	ShouldEmbedBlockChanges(bc *BlockChanges) bool
}

// KeyServer fetches/writes server-side key halves from/to the key server.
type KeyServer interface {
	// GetTLFCryptKeyServerHalf gets a server-side key half for a
	// device given the key half ID.
	GetTLFCryptKeyServerHalf(ctx context.Context,
		serverHalfID TLFCryptKeyServerHalfID,
		cryptPublicKey kbfscrypto.CryptPublicKey) (
		kbfscrypto.TLFCryptKeyServerHalf, error)

	// PutTLFCryptKeyServerHalves stores a server-side key halves for a
	// set of users and devices.
	PutTLFCryptKeyServerHalves(ctx context.Context,
		serverKeyHalves map[keybase1.UID]map[keybase1.KID]kbfscrypto.TLFCryptKeyServerHalf) error

	// DeleteTLFCryptKeyServerHalf deletes a server-side key half for a
	// device given the key half ID.
	DeleteTLFCryptKeyServerHalf(ctx context.Context,
		uid keybase1.UID, kid keybase1.KID,
		serverHalfID TLFCryptKeyServerHalfID) error

	// Shutdown is called to free any KeyServer resources.
	Shutdown()
}

// NodeChange represents a change made to a node as part of an atomic
// file system operation.
type NodeChange struct {
	Node Node
	// Basenames of entries added/removed.
	DirUpdated  []string
	FileUpdated []WriteRange
}

// Observer can be notified that there is an available update for a
// given directory.  The notification callbacks should not block, or
// make any calls to the Notifier interface.  Nodes passed to the
// observer should not be held past the end of the notification
// callback.
type Observer interface {
	// LocalChange announces that the file at this Node has been
	// updated locally, but not yet saved at the server.
	LocalChange(ctx context.Context, node Node, write WriteRange)
	// BatchChanges announces that the nodes have all been updated
	// together atomically.  Each NodeChange in changes affects the
	// same top-level folder and branch.
	BatchChanges(ctx context.Context, changes []NodeChange)
	// TlfHandleChange announces that the handle of the corresponding
	// folder branch has changed, likely due to previously-unresolved
	// assertions becoming resolved.  This indicates that the listener
	// should switch over any cached paths for this folder-branch to
	// the new name.  Nodes that were acquired under the old name will
	// still continue to work, but new lookups on the old name may
	// either encounter alias errors or entirely new TLFs (in the case
	// of conflicts).
	TlfHandleChange(ctx context.Context, newHandle *TlfHandle)
}

// Notifier notifies registrants of directory changes
type Notifier interface {
	// RegisterForChanges declares that the given Observer wants to
	// subscribe to updates for the given top-level folders.
	RegisterForChanges(folderBranches []FolderBranch, obs Observer) error
	// UnregisterFromChanges declares that the given Observer no
	// longer wants to subscribe to updates for the given top-level
	// folders.
	UnregisterFromChanges(folderBranches []FolderBranch, obs Observer) error
}

// Clock is an interface for getting the current time
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// ConflictRenamer deals with names for conflicting directory entries.
type ConflictRenamer interface {
	// ConflictRename returns the appropriately modified filename.
	ConflictRename(op op, original string) string
}

// Config collects all the singleton instance instantiations needed to
// run KBFS in one place.  The methods below are self-explanatory and
// do not require comments.
type Config interface {
	KBFSOps() KBFSOps
	SetKBFSOps(KBFSOps)
	KBPKI() KBPKI
	SetKBPKI(KBPKI)
	KeyManager() KeyManager
	SetKeyManager(KeyManager)
	Reporter() Reporter
	SetReporter(Reporter)
	MDCache() MDCache
	SetMDCache(MDCache)
	KeyCache() KeyCache
	SetKeyCache(KeyCache)
	BlockCache() BlockCache
	SetBlockCache(BlockCache)
	DirtyBlockCache() DirtyBlockCache
	SetDirtyBlockCache(DirtyBlockCache)
	Crypto() Crypto
	SetCrypto(Crypto)
	Codec() kbfscodec.Codec
	SetCodec(kbfscodec.Codec)
	MDOps() MDOps
	SetMDOps(MDOps)
	KeyOps() KeyOps
	SetKeyOps(KeyOps)
	BlockOps() BlockOps
	SetBlockOps(BlockOps)
	MDServer() MDServer
	SetMDServer(MDServer)
	BlockServer() BlockServer
	SetBlockServer(BlockServer)
	KeyServer() KeyServer
	SetKeyServer(KeyServer)
	KeybaseService() KeybaseService
	SetKeybaseService(KeybaseService)
	BlockSplitter() BlockSplitter
	SetBlockSplitter(BlockSplitter)
	Notifier() Notifier
	SetNotifier(Notifier)
	Clock() Clock
	SetClock(Clock)
	ConflictRenamer() ConflictRenamer
	SetConflictRenamer(ConflictRenamer)
	MetadataVersion() MetadataVer
	DataVersion() DataVer
	RekeyQueue() RekeyQueue
	SetRekeyQueue(RekeyQueue)
	// ReqsBufSize indicates the number of read or write operations
	// that can be buffered per folder
	ReqsBufSize() int
	// MaxFileBytes indicates the maximum supported plaintext size of
	// a file in bytes.
	MaxFileBytes() uint64
	// MaxNameBytes indicates the maximum supported size of a
	// directory entry name in bytes.
	MaxNameBytes() uint32
	// MaxDirBytes indicates the maximum supported plaintext size of a
	// directory in bytes.
	MaxDirBytes() uint64
	// DoBackgroundFlushes says whether we should periodically try to
	// flush dirty files, even without a sync from the user.  Should
	// be true except for during some testing.
	DoBackgroundFlushes() bool
	SetDoBackgroundFlushes(bool)
	// RekeyWithPromptWaitTime indicates how long to wait, after
	// setting the rekey bit, before prompting for a paper key.
	RekeyWithPromptWaitTime() time.Duration

	// GracePeriod specifies a grace period for which a delayed cancellation
	// waits before actual cancels the context. This is useful for giving
	// critical portion of a slow remote operation some extra time to finish as
	// an effort to avoid conflicting. Example include an O_EXCL Create call
	// interrupted by ALRM signal actually makes it to the server, while
	// application assumes not since EINTR is returned. A delayed cancellation
	// allows us to distinguish between successful cancel (where remote operation
	// didn't make to server) or failed cancel (where remote operation made to
	// the server). However, the optimal value of this depends on the network
	// conditions. A long grace period for really good network condition would
	// just unnecessarily slow down Ctrl-C.
	//
	// TODO: make this adaptive and self-change over time based on network
	// conditions.
	DelayedCancellationGracePeriod() time.Duration
	SetDelayedCancellationGracePeriod(time.Duration)
	// QuotaReclamationPeriod indicates how often should each TLF
	// should check for quota to reclaim.  If the Duration.Seconds()
	// == 0, quota reclamation should not run automatically.
	QuotaReclamationPeriod() time.Duration
	// QuotaReclamationMinUnrefAge indicates the minimum time a block
	// must have been unreferenced before it can be reclaimed.
	QuotaReclamationMinUnrefAge() time.Duration
	// QuotaReclamationMinHeadAge indicates the minimum age of the
	// most recently merged MD update before we can run reclamation,
	// to avoid conflicting with a currently active writer.
	QuotaReclamationMinHeadAge() time.Duration

	// ResetCaches clears and re-initializes all data and key caches.
	ResetCaches()

	MakeLogger(module string) logger.Logger
	SetLoggerMaker(func(module string) logger.Logger)
	// MetricsRegistry may be nil, which should be interpreted as
	// not using metrics at all. (i.e., as if UseNilMetrics were
	// set). This differs from how go-metrics treats nil Registry
	// objects, which is to use the default registry.
	MetricsRegistry() metrics.Registry
	SetMetricsRegistry(metrics.Registry)
	// TLFValidDuration is the time TLFs are valid before identification needs to be redone.
	TLFValidDuration() time.Duration
	// SetTLFValidDuration sets TLFValidDuration.
	SetTLFValidDuration(time.Duration)
	// Shutdown is called to free config resources.
	Shutdown() error
	// CheckStateOnShutdown tells the caller whether or not it is safe
	// to check the state of the system on shutdown.
	CheckStateOnShutdown() bool
}

// NodeCache holds Nodes, and allows libkbfs to update them when
// things change about the underlying KBFS blocks.  It is probably
// most useful to instantiate this on a per-folder-branch basis, so
// that it can create a Path with the correct DirId and Branch name.
type NodeCache interface {
	// GetOrCreate either makes a new Node for the given
	// BlockPointer, or returns an existing one. TODO: If we ever
	// support hard links, we will have to revisit the "name" and
	// "parent" parameters here.  name must not be empty. Returns
	// an error if parent cannot be found.
	GetOrCreate(ptr BlockPointer, name string, parent Node) (Node, error)
	// Get returns the Node associated with the given ptr if one
	// already exists.  Otherwise, it returns nil.
	Get(ref blockRef) Node
	// UpdatePointer updates the BlockPointer for the corresponding
	// Node.  NodeCache ignores this call when oldRef is not cached in
	// any Node.
	UpdatePointer(oldRef blockRef, newPtr BlockPointer)
	// Move swaps the parent node for the corresponding Node, and
	// updates the node's name.  NodeCache ignores the call when ptr
	// is not cached.  Returns an error if newParent cannot be found.
	// If newParent is nil, it treats the ptr's corresponding node as
	// being unlinked from the old parent completely.
	Move(ref blockRef, newParent Node, newName string) error
	// Unlink set the corresponding node's parent to nil and caches
	// the provided path in case the node is still open. NodeCache
	// ignores the call when ptr is not cached.  The path is required
	// because the caller may have made changes to the parent nodes
	// already that shouldn't be reflected in the cached path.
	Unlink(ref blockRef, oldPath path)
	// PathFromNode creates the path up to a given Node.
	PathFromNode(node Node) path
}

// fileBlockDeepCopier fetches a file block, makes a deep copy of it
// (duplicating pointer for any indirect blocks) and generates a new
// random temporary block ID for it.  It returns the new BlockPointer,
// and internally saves the block for future uses.
type fileBlockDeepCopier func(context.Context, string, BlockPointer) (
	BlockPointer, error)

// crAction represents a specific action to take as part of the
// conflict resolution process.
type crAction interface {
	// swapUnmergedBlock should be called before do(), and if it
	// returns true, the caller must use the merged block
	// corresponding to the returned BlockPointer instead of
	// unmergedBlock when calling do().  If BlockPointer{} is zeroPtr
	// (and true is returned), just swap in the regular mergedBlock.
	swapUnmergedBlock(unmergedChains *crChains, mergedChains *crChains,
		unmergedBlock *DirBlock) (bool, BlockPointer, error)
	// do modifies the given merged block in place to resolve the
	// conflict, and potential uses the provided blockCopyFetchers to
	// obtain copies of other blocks (along with new BlockPointers)
	// when requiring a block copy.
	do(ctx context.Context, unmergedCopier fileBlockDeepCopier,
		mergedCopier fileBlockDeepCopier, unmergedBlock *DirBlock,
		mergedBlock *DirBlock) error
	// updateOps potentially modifies, in place, the slices of
	// unmerged and merged operations stored in the corresponding
	// crChains for the given unmerged and merged most recent
	// pointers.  Eventually, the "unmerged" ops will be pushed as
	// part of a MD update, and so should contain any necessarily
	// operations to fully merge the unmerged data, including any
	// conflict resolution.  The "merged" ops will be played through
	// locally, to notify any caches about the newly-obtained merged
	// data (and any changes to local data that were required as part
	// of conflict resolution, such as renames).  A few things to note:
	// * A particular action's updateOps method may be called more than
	//   once for different sets of chains, however it should only add
	//   new directory operations (like create/rm/rename) into directory
	//   chains.
	// * updateOps doesn't necessarily result in correct BlockPointers within
	//   each of those ops; that must happen in a later phase.
	// * mergedBlock can be nil if the chain is for a file.
	updateOps(unmergedMostRecent BlockPointer, mergedMostRecent BlockPointer,
		unmergedBlock *DirBlock, mergedBlock *DirBlock,
		unmergedChains *crChains, mergedChains *crChains) error
	// String returns a string representation for this crAction, used
	// for debugging.
	String() string
}

// RekeyQueue is a managed queue of folders needing some rekey action taken upon them
// by the current client.
type RekeyQueue interface {
	// Enqueue enqueues a folder for rekey action.
	Enqueue(TlfID) <-chan error
	// IsRekeyPending returns true if the given folder is in the rekey queue.
	IsRekeyPending(TlfID) bool
	// GetRekeyChannel will return any rekey completion channel (if pending.)
	GetRekeyChannel(id TlfID) <-chan error
	// Clear cancels all pending rekey actions and clears the queue.
	Clear()
	// Waits for all queued rekeys to finish
	Wait(ctx context.Context) error
}

// BareRootMetadata is a read-only interface to the bare serializeable MD that
// is signed by the reader or writer.
type BareRootMetadata interface {
	// TlfID returns the ID of the TLF this BareRootMetadata is for.
	TlfID() TlfID
	// LatestKeyGeneration returns the most recent key generation in this
	// BareRootMetadata, or PublicKeyGen if this TLF is public.
	LatestKeyGeneration() KeyGen
	// IsValidRekeyRequest returns true if the current block is a simple rekey wrt
	// the passed block.
	IsValidRekeyRequest(codec kbfscodec.Codec, prevMd BareRootMetadata,
		user keybase1.UID, prevExtra, extra ExtraMetadata) (bool, error)
	// MergedStatus returns the status of this update -- has it been
	// merged into the main folder or not?
	MergedStatus() MergeStatus
	// IsRekeySet returns true if the rekey bit is set.
	IsRekeySet() bool
	// IsWriterMetadataCopiedSet returns true if the bit is set indicating
	// the writer metadata was copied.
	IsWriterMetadataCopiedSet() bool
	// IsFinal returns true if this is the last metadata block for a given
	// folder.  This is only expected to be set for folder resets.
	IsFinal() bool
	// IsWriter returns whether or not the user+device is an authorized writer.
	IsWriter(user keybase1.UID, deviceKID keybase1.KID, extra ExtraMetadata) bool
	// IsReader returns whether or not the user+device is an authorized reader.
	IsReader(user keybase1.UID, deviceKID keybase1.KID, extra ExtraMetadata) bool
	// DeepCopy returns a deep copy of the underlying data structure.
	DeepCopy(codec kbfscodec.Codec) (BareRootMetadata, error)
	// MakeSuccessorCopy returns a newly constructed successor copy to this metadata revision.
	// It differs from DeepCopy in that it can perform an up conversion to a new metadata
	// version.
	MakeSuccessorCopy(codec kbfscodec.Codec) (BareRootMetadata, error)
	// CheckValidSuccessor makes sure the given BareRootMetadata is a valid
	// successor to the current one, and returns an error otherwise.
	CheckValidSuccessor(currID MdID, nextMd BareRootMetadata) error
	// CheckValidSuccessorForServer is like CheckValidSuccessor but with
	// server-specific error messages.
	CheckValidSuccessorForServer(currID MdID, nextMd BareRootMetadata) error
	// MakeBareTlfHandle makes a BareTlfHandle for this
	// BareRootMetadata. Should be used only by servers and MDOps.
	MakeBareTlfHandle(extra ExtraMetadata) (BareTlfHandle, error)
	// TlfHandleExtensions returns a list of handle extensions associated with the TLf.
	TlfHandleExtensions() (extensions []TlfHandleExtension)
	// GetDeviceKIDs returns the KIDs (of
	// kbfscrypto.CryptPublicKeys) for all known devices for the
	// given user at the given key generation, if any.  Returns an
	// error if the TLF is public, or if the given key generation
	// is invalid.
	GetDeviceKIDs(keyGen KeyGen, user keybase1.UID, extra ExtraMetadata) (
		[]keybase1.KID, error)
	// HasKeyForUser returns whether or not the given user has keys for at
	// least one device at the given key generation. Returns false if the
	// TLF is public, or if the given key generation is invalid. Equivalent to:
	//
	//   kids, err := GetDeviceKIDs(keyGen, user)
	//   return (err == nil) && (len(kids) > 0)
	HasKeyForUser(keyGen KeyGen, user keybase1.UID, extra ExtraMetadata) bool
	// GetTLFCryptKeyParams returns all the necessary info to construct
	// the TLF crypt key for the given key generation, user, and device
	// (identified by its crypt public key), or false if not found. This
	// returns an error if the TLF is public.
	GetTLFCryptKeyParams(keyGen KeyGen, user keybase1.UID,
		key kbfscrypto.CryptPublicKey, extra ExtraMetadata) (
		kbfscrypto.TLFEphemeralPublicKey,
		EncryptedTLFCryptKeyClientHalf,
		TLFCryptKeyServerHalfID, bool, error)
	// IsValidAndSigned verifies the BareRootMetadata, checks the
	// writer signature, and returns an error if a problem was
	// found. This should be the first thing checked on a BRMD
	// retrieved from an untrusted source, and then the signing
	// user and key should be validated, either by comparing to
	// the current device key (using IsLastModifiedBy), or by
	// checking with KBPKI.
	IsValidAndSigned(codec kbfscodec.Codec,
		crypto cryptoPure, extra ExtraMetadata) error
	// IsLastModifiedBy verifies that the BareRootMetadata is
	// written by the given user and device (identified by the KID
	// of the device verifying key), and returns an error if not.
	IsLastModifiedBy(uid keybase1.UID, key kbfscrypto.VerifyingKey) error
	// LastModifyingWriter return the UID of the last user to modify the writer metadata.
	LastModifyingWriter() keybase1.UID
	// LastModifyingWriterKID returns the KID of the last device to modify the writer metadata.
	LastModifyingWriterKID() keybase1.KID
	// LastModifyingUser return the UID of the last user to modify the any of the metadata.
	GetLastModifyingUser() keybase1.UID
	// RefBytes returns the number of newly referenced bytes introduced by this revision of metadata.
	RefBytes() uint64
	// UnrefBytes returns the number of newly unreferenced bytes introduced by this revision of metadata.
	UnrefBytes() uint64
	// DiskUsage returns the estimated disk usage for the folder as of this revision of metadata.
	DiskUsage() uint64
	// RevisionNumber returns the revision number associated with this metadata structure.
	RevisionNumber() MetadataRevision
	// BID returns the per-device branch ID associated with this metadata revision.
	BID() BranchID
	// GetPrevRoot returns the hash of the previous metadata revision.
	GetPrevRoot() MdID
	// IsUnmergedSet returns true if the unmerged bit is set.
	IsUnmergedSet() bool
	// GetSerializedPrivateMetadata returns the serialized private metadata as a byte slice.
	GetSerializedPrivateMetadata() []byte
	// GetSerializedWriterMetadata serializes the underlying writer metadata and returns the result.
	GetSerializedWriterMetadata(codec kbfscodec.Codec) ([]byte, error)
	// GetWriterMetadataSigInfo returns the signature info associated with the the writer metadata.
	GetWriterMetadataSigInfo() kbfscrypto.SignatureInfo
	// Version returns the metadata version.
	Version() MetadataVer
	// GetTLFPublicKey returns the TLF public key for the give key generation.
	// Note the *TLFWriterKeyBundleV3 is expected to be nil for pre-v3 metadata.
	GetTLFPublicKey(KeyGen, ExtraMetadata) (kbfscrypto.TLFPublicKey, bool)
	// AreKeyGenerationsEqual returns true if all key generations in the passed metadata are equal to those
	// in this revision.
	AreKeyGenerationsEqual(kbfscodec.Codec, BareRootMetadata) (bool, error)
	// GetUnresolvedParticipants returns any unresolved readers and writers present in this revision of metadata.
	GetUnresolvedParticipants() (readers, writers []keybase1.SocialAssertion)
	// GetTLFWriterKeyBundleID returns the ID of the externally-stored writer key bundle, or the zero value if
	// this object stores it internally.
	GetTLFWriterKeyBundleID() TLFWriterKeyBundleID
	// GetTLFReaderKeyBundleID returns the ID of the externally-stored reader key bundle, or the zero value if
	// this object stores it internally.
	GetTLFReaderKeyBundleID() TLFReaderKeyBundleID
	// StoresHistoricTLFCryptKeys returns whether or not history keys are symmetrically encrypted; if not, they're
	// encrypted per-device.
	StoresHistoricTLFCryptKeys() bool
	// GetHistoricTLFCryptKey attempts to symmetrically decrypt the key at the given
	// generation using the current generation's TLFCryptKey.
	GetHistoricTLFCryptKey(c cryptoPure, keyGen KeyGen,
		currentKey kbfscrypto.TLFCryptKey, extra ExtraMetadata) (
		kbfscrypto.TLFCryptKey, error)
}

// MutableBareRootMetadata is a mutable interface to the bare serializeable MD that is signed by the reader or writer.
type MutableBareRootMetadata interface {
	BareRootMetadata

	// SetRefBytes sets the number of newly referenced bytes introduced by this revision of metadata.
	SetRefBytes(refBytes uint64)
	// SetUnrefBytes sets the number of newly unreferenced bytes introduced by this revision of metadata.
	SetUnrefBytes(unrefBytes uint64)
	// SetDiskUsage sets the estimated disk usage for the folder as of this revision of metadata.
	SetDiskUsage(diskUsage uint64)
	// AddRefBytes increments the number of newly referenced bytes introduced by this revision of metadata.
	AddRefBytes(refBytes uint64)
	// AddUnrefBytes increments the number of newly unreferenced bytes introduced by this revision of metadata.
	AddUnrefBytes(unrefBytes uint64)
	// AddDiskUsage increments the estimated disk usage for the folder as of this revision of metadata.
	AddDiskUsage(diskUsage uint64)
	// ClearRekeyBit unsets any set rekey bit.
	ClearRekeyBit()
	// ClearWriterMetadataCopiedBit unsets any set writer metadata copied bit.
	ClearWriterMetadataCopiedBit()
	// ClearFinalBit unsets any final bit.
	ClearFinalBit()
	// SetUnmerged sets the unmerged bit.
	SetUnmerged()
	// SetBranchID sets the branch ID for this metadata revision.
	SetBranchID(bid BranchID)
	// SetPrevRoot sets the hash of the previous metadata revision.
	SetPrevRoot(mdID MdID)
	// SetSerializedPrivateMetadata sets the serialized private metadata.
	SetSerializedPrivateMetadata(spmd []byte)
	// SetWriterMetadataSigInfo sets the signature info associated with the the writer metadata.
	SetWriterMetadataSigInfo(sigInfo kbfscrypto.SignatureInfo)
	// SetLastModifyingWriter sets the UID of the last user to modify the writer metadata.
	SetLastModifyingWriter(user keybase1.UID)
	// SetLastModifyingUser sets the UID of the last user to modify any of the metadata.
	SetLastModifyingUser(user keybase1.UID)
	// SetRekeyBit sets the rekey bit.
	SetRekeyBit()
	// SetFinalBit sets the finalized bit.
	SetFinalBit()
	// SetWriterMetadataCopiedBit set the writer metadata copied bit.
	SetWriterMetadataCopiedBit()
	// SetRevision sets the revision number of the underlying metadata.
	SetRevision(revision MetadataRevision)
	// AddNewKeysForTesting adds new writer and reader TLF key bundles to this revision of metadata.
	// Note: This is only used for testing at the moment.
	AddNewKeysForTesting(crypto cryptoPure, wDkim, rDkim UserDeviceKeyInfoMap) (ExtraMetadata, error)
	// NewKeyGeneration adds a new key generation to this revision of metadata.
	NewKeyGeneration(pubKey kbfscrypto.TLFPublicKey) (extra ExtraMetadata)
	// SetUnresolvedReaders sets the list of unresolved readers assoiated with this folder.
	SetUnresolvedReaders(readers []keybase1.SocialAssertion)
	// SetUnresolvedWriters sets the list of unresolved writers assoiated with this folder.
	SetUnresolvedWriters(writers []keybase1.SocialAssertion)
	// SetConflictInfo sets any conflict info associated with this metadata revision.
	SetConflictInfo(ci *TlfHandleExtension)
	// SetFinalizedInfo sets any finalized info associated with this metadata revision.
	SetFinalizedInfo(fi *TlfHandleExtension)
	// SetWriters sets the list of writers associated with this folder.
	SetWriters(writers []keybase1.UID)
	// SetTlfID sets the ID of the underlying folder in the metadata structure.
	SetTlfID(tlf TlfID)
	// FakeInitialRekey fakes the initial rekey for the given
	// BareRootMetadata. This is necessary since newly-created
	// BareRootMetadata objects don't have enough data to build a
	// TlfHandle from until the first rekey.
	FakeInitialRekey(c cryptoPure, h BareTlfHandle) (ExtraMetadata, error)
	// Update initializes the given freshly-created BareRootMetadata object with
	// the given TlfID and BareTlfHandle. Note that if the given ID/handle are private,
	// rekeying must be done separately.
	Update(tlf TlfID, h BareTlfHandle) error
	// Returns the TLF key bundles for this metadata at the given key generation.
	// MDv3 TODO: Get rid of this.
	GetTLFKeyBundles(keyGen KeyGen) (*TLFWriterKeyBundleV2, *TLFReaderKeyBundleV2, error)
	// GetUserDeviceKeyInfoMaps returns the given user device key info maps for the given
	// key generation.
	GetUserDeviceKeyInfoMaps(keyGen KeyGen, extra ExtraMetadata) (
		readers, writers UserDeviceKeyInfoMap, err error)
	// FinalizeRekey is called after all rekeying work has been performed on the underlying
	// metadata.
	FinalizeRekey(c cryptoPure, prevKey,
		key kbfscrypto.TLFCryptKey, extra ExtraMetadata) error
}

// KeyBundleCache is an interface to a key bundle cache for use with v3 metadata.
type KeyBundleCache interface {
	// GetTLFReaderKeyBundle returns the TLFReaderKeyBundleV2 for the given TLFReaderKeyBundleID.
	GetTLFReaderKeyBundle(TLFReaderKeyBundleID) (TLFReaderKeyBundleV3, bool)
	// GetTLFWriterKeyBundle returns the TLFWriterKeyBundleV3 for the given TLFWriterKeyBundleID.
	GetTLFWriterKeyBundle(TLFWriterKeyBundleID) (TLFWriterKeyBundleV3, bool)
	// PutTLFReaderKeyBundle stores the given TLFReaderKeyBundleV2.
	PutTLFReaderKeyBundle(TLFReaderKeyBundleV3)
	// PutTLFWriterKeyBundle stores the given TLFWriterKeyBundleV3.
	PutTLFWriterKeyBundle(TLFWriterKeyBundleV3)
}

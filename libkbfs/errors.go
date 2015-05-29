package libkbfs

import (
	"fmt"
	"syscall"

	"bazil.org/fuse"

	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/protocol/go"
)

var ErrorFile string = ".kbfs_error"

type WrapError struct {
	Err error
}

func (e *WrapError) String() string {
	return e.Err.Error()
}

type NameExistsError struct {
	Name string
}

func (e *NameExistsError) Error() string {
	return fmt.Sprintf("%s already exists", e.Name)
}

type NoSuchNameError struct {
	Name string
}

func (e *NoSuchNameError) Error() string {
	return fmt.Sprintf("%s doesn't exist", e.Name)
}

type BadPathError struct {
	Name string
}

func (e *BadPathError) Error() string {
	return fmt.Sprintf("%s is in an incorrect format", e.Name)
}

type DirNotEmptyError struct {
	Name string
}

func (e *DirNotEmptyError) Error() string {
	return fmt.Sprintf("Directory %s is not empty and can't be removed", e.Name)
}

var _ fuse.ErrorNumber = (*DirNotEmptyError)(nil)

func (e *DirNotEmptyError) Errno() fuse.Errno {
	return fuse.Errno(syscall.ENOTEMPTY)
}

type TopDirAccessError struct {
	Name Path
}

func (e *TopDirAccessError) Error() string {
	return fmt.Sprintf("Operation not permitted on folder %s", e.Name.TopDir)
}

type RenameAcrossDirsError struct {
}

func (e *RenameAcrossDirsError) Error() string {
	return fmt.Sprintf("Cannot rename across directories")
}

type ErrorFileAccessError struct {
}

func (e *ErrorFileAccessError) Error() string {
	return fmt.Sprintf("Operation not allowed on file %s", ErrorFile)
}

type ReadAccessError struct {
	User string
	Dir  string
}

func (e *ReadAccessError) Error() string {
	return fmt.Sprintf("%s does not have read access to directory %s",
		e.User, e.Dir)
}

type WriteAccessError struct {
	User string
	Dir  string
}

func (e *WriteAccessError) Error() string {
	return fmt.Sprintf("%s does not have write access to directory %s",
		e.User, e.Dir)
}

func NewReadAccessError(config Config, dir *DirHandle, uid keybase1.UID) error {
	dirname := dir.ToString(config)
	if u, err2 := config.KBPKI().GetUser(uid); err2 == nil {
		return &ReadAccessError{u.GetName(), dirname}
	} else {
		return &ReadAccessError{uid.String(), dirname}
	}
}

func NewWriteAccessError(config Config, dir *DirHandle, uid keybase1.UID) error {
	dirname := dir.ToString(config)
	if u, err2 := config.KBPKI().GetUser(uid); err2 == nil {
		return &WriteAccessError{u.GetName(), dirname}
	} else {
		return &WriteAccessError{uid.String(), dirname}
	}
}

type NotDirError struct {
	Path Path
}

func (e *NotDirError) Error() string {
	return fmt.Sprintf("%s is not a directory (in folder %s)",
		&e.Path, e.Path.TopDir)
}

type NotFileError struct {
	Path Path
}

func (e *NotFileError) Error() string {
	return fmt.Sprintf("%s is not a file (folder %s)", e.Path, e.Path.TopDir)
}

type BadDataError struct {
	Id BlockId
}

func (e *BadDataError) Error() string {
	return fmt.Sprintf("Bad data for block %v", e.Id)
}

type NoSuchBlockError struct {
	Id BlockId
}

func (e *NoSuchBlockError) Error() string {
	return fmt.Sprintf("Couldn't get block %v", e.Id)
}

type FinalizeError struct {
	Id BlockId
}

func (e *FinalizeError) Error() string {
	return fmt.Sprintf("No need to finalize block %v; not dirty", e.Id)
}

type BadCryptoError struct {
	Id BlockId
}

func (e *BadCryptoError) Error() string {
	return fmt.Sprintf("Bad crypto for block %v", e.Id)
}

type BadCryptoMDError struct {
	Id DirId
}

func (e *BadCryptoMDError) Error() string {
	return fmt.Sprintf("Bad crypto for the metadata of directory %v", e.Id)
}

type BadMDError struct {
	Id MDId
}

func (e *BadMDError) Error() string {
	return fmt.Sprintf("Wrong format for metadata for directory %v", e.Id)
}

type MDMismatchError struct {
	Dir string
	Err string
}

func (e *MDMismatchError) Error() string {
	return fmt.Sprintf("Could not verify metadata for directory %s: %s",
		e.Dir, e.Err)
}

type NoSuchMDError struct {
	Id MDId
}

func (e *NoSuchMDError) Error() string {
	return fmt.Sprintf("Couldn't get metadata for %v", e.Id)
}

type NewVersionError struct {
	Path Path
	Ver  Ver
}

func (e *NewVersionError) Error() string {
	return fmt.Sprintf(
		"The data at path %s is of a version (%d) that we can't read "+
			"(in folder %s)",
		e.Path, e.Ver, e.Path.TopDir)
}

type NewKeyVersionError struct {
	Path   string
	KeyVer KeyVer
}

func (e *NewKeyVersionError) Error() string {
	return fmt.Sprintf(
		"The data at path %s is keyed with a key version (%d) that "+
			"we don't know", e.Path, e.KeyVer)
}

type BadSplitError struct {
}

func (e *BadSplitError) Error() string {
	return "Unexpected bad block split"
}

type LoggedInUserError struct {
}

func (e *LoggedInUserError) Error() string {
	return "No UID for logged-in user"
}

type InconsistentByteCountError struct {
	ExpectedByteCount int
	ByteCount         int
}

func (e *InconsistentByteCountError) Error() string {
	return fmt.Sprintf("Expected %d bytes, got %d bytes", e.ExpectedByteCount, e.ByteCount)
}

type TooHighByteCountError struct {
	ExpectedMaxByteCount int
	ByteCount            int
}

func (e *TooHighByteCountError) Error() string {
	return fmt.Sprintf("Expected at most %d bytes, got %d bytes", e.ExpectedMaxByteCount, e.ByteCount)
}

type TooLowByteCountError struct {
	ExpectedMinByteCount int
	ByteCount            int
}

func (e *TooLowByteCountError) Error() string {
	return fmt.Sprintf("Expected at least %d bytes, got %d bytes", e.ExpectedMinByteCount, e.ByteCount)
}

type InconsistentBlockPointerError struct {
	Ptr BlockPointer
}

func (e *InconsistentBlockPointerError) Error() string {
	return fmt.Sprintf("Block pointer to dirty block %v with non-zero quota size = %d bytes", e.Ptr.Id, e.Ptr.QuotaSize)
}

type WriteNeededInReadRequest struct {
}

func (e *WriteNeededInReadRequest) Error() string {
	return "This request needs exclusive access, but doesn't have it."
}

type KeyNotFoundError struct {
	kid libkb.KID
}

func (e KeyNotFoundError) Error() string {
	return fmt.Sprintf("Could not find key with kid=%s", e.kid)
}

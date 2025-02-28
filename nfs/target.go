// Copyright © 2017 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: BSD-2-Clause
//
package nfs

import (
	"fmt"
	"io"
	"os"
	_path "path"
	"strings"

	"github.com/go-nfs/nfsv3/nfs/rpc"
	"github.com/go-nfs/nfsv3/nfs/util"
	"github.com/go-nfs/nfsv3/nfs/xdr"
)

type Target struct {
	*rpc.Client

	auth    rpc.Auth
	fh      []byte
	dirPath string
	fsinfo  *FSInfo
}

func NewTarget(addr string, auth rpc.Auth, fh []byte, dirpath string, priv bool) (*Target, error) {
	m := rpc.Mapping{
		Prog: Nfs3Prog,
		Vers: Nfs3Vers,
		Prot: rpc.IPProtoTCP,
		Port: 0,
	}

	client, err := DialService(addr, m, priv)
	if err != nil {
		return nil, err
	}

	return NewTargetWithClient(client, auth, fh, dirpath)
}

func NewTargetWithClient(client *rpc.Client, auth rpc.Auth, fh []byte, dirpath string) (*Target, error) {
	vol := &Target{
		Client:  client,
		auth:    auth,
		fh:      fh,
		dirPath: dirpath,
	}

	fsinfo, err := vol.FSInfo()
	if err != nil {
		return nil, err
	}

	vol.fsinfo = fsinfo
	util.Debugf("%s fsinfo=%#v", dirpath, fsinfo)

	return vol, nil
}

// wraps the Call function to check status and decode errors
func (v *Target) call(c interface{}) (io.ReadSeeker, error) {
	res, err := v.Call(c)
	if err != nil {
		return nil, err
	}

	status, err := xdr.ReadUint32(res)
	if err != nil {
		return nil, err
	}

	if err = NFS3Error(status); err != nil {
		return nil, err
	}

	return res, nil
}

func (v *Target) FSInfo() (*FSInfo, error) {
	type FSInfoArgs struct {
		rpc.Header
		FsRoot []byte
	}

	res, err := v.call(&FSInfoArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3FSInfo,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		FsRoot: v.fh,
	})

	if err != nil {
		util.Debugf("fsroot: %s", err.Error())
		return nil, err
	}

	fsinfo := new(FSInfo)
	if err = xdr.Read(res, fsinfo); err != nil {
		return nil, err
	}

	return fsinfo, nil
}

func sameHandle(a []byte, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i, av := range a {
		if b[i] != av {
			return false
		}
	}
	return true
}

// Lookup returns attributes and the file handle to a given dirent
func (v *Target) Lookup(p string) (os.FileInfo, []byte, error) {
	fattr, fh, _, _, err := v.lookupInner(v.fh, p, true, nil)
	return fattr, fh, err
}

func (v *Target) lookupInner(fh []byte, p string, lookupLast bool, lookupOrigin []byte) (*Fattr, []byte, string, []byte, error) {
	var (
		err   error
		fattr *Fattr
	)

	// desecend down a path heirarchy to get the last elem's fh
	dirents := strings.Split(p, "/")
	var dirent string
	var prevFh []byte
	for i := 0; i < len(dirents); {
		dirent = dirents[i]
		prevFh = fh
		i += 1
		if i == len(dirents) && !lookupLast {
			fattr = nil
			fh = nil
			break
		}
		// we're assuming the root is always the root of the mount
		if dirent == "." || dirent == "" {
			util.Debugf("root -> 0x%x", fh)
			continue
		}
		fattr, fh, _, err = v.lookup(prevFh, dirent)
		if err != nil {
			return nil, nil, "", nil, err
		}
		if fattr.FileMode&0o170000 == 0o120000 {
			if lookupOrigin != nil && sameHandle(fh, lookupOrigin) {
				return nil, nil, "", nil, fmt.Errorf("recursed symlink")
			}
			// symlink
			_, target, err := v.readlinkFh(fh)
			if err != nil {
				return nil, nil, "", nil, err
			}
			// reparse
			_, fh, _, _, err = v.lookupInner(v.fh, target, true, fh)
		}
	}

	return fattr, fh, dirent, prevFh, nil
}

// lookup returns the same as above, but by fh and name
func (v *Target) lookup(fh []byte, name string) (*Fattr, []byte, *Fattr, error) {
	type Lookup3Args struct {
		rpc.Header
		What Diropargs3
	}

	type LookupOk struct {
		FH      []byte
		Attr    PostOpAttr
		DirAttr PostOpAttr
	}

	res, err := v.call(&Lookup3Args{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Lookup,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		What: Diropargs3{
			FH:       fh,
			Filename: name,
		},
	})

	if err != nil {
		util.Debugf("lookup(%s): %s", name, err.Error())
		return nil, nil, nil, err
	}

	lookupres := new(LookupOk)
	if err := xdr.Read(res, lookupres); err != nil {
		util.Errorf("lookup(%s) failed to parse return: %s", name, err)
		util.Debugf("lookup partial decode: %+v", *lookupres)
		return nil, nil, nil, err
	}

	util.Debugf("lookup(%s): FH 0x%x, attr: %+v", name, lookupres.FH, lookupres.Attr.Attr)
	return &lookupres.Attr.Attr, lookupres.FH, &lookupres.DirAttr.Attr, nil
}

// Access file
func (v *Target) Access(path string, mode uint32) (uint32, error) {

	_, fh, err := v.Lookup(path)
	if err != nil {
		return 0, err
	}

	_, mode, err = v.access(fh, path, mode)

	return mode, err
}

// access returns the same as above, but by fh and name
func (v *Target) access(fh []byte, path string, access uint32) (*Fattr, uint32, error) {
	type Access3Args struct {
		rpc.Header
		FH     []byte
		Access uint32
	}

	type AccessOk struct {
		Attr   PostOpAttr
		Access uint32
	}

	res, err := v.call(&Access3Args{Header: rpc.Header{
		Rpcvers: 2,
		Prog:    Nfs3Prog,
		Vers:    Nfs3Vers,
		Proc:    NFSProc3Access,
		Cred:    v.auth,
		Verf:    rpc.AuthNull,
	},
		FH:     fh,
		Access: access})

	if err != nil {
		util.Debugf("access(%s): %s", path, err.Error())
		return nil, 0, err
	}

	accessres := new(AccessOk)

	if err := xdr.Read(res, accessres); err != nil {
		util.Errorf("access(%s) failed to parse return: %s", path, err)
		util.Debugf("access partial decode: %+v", *accessres)
		return nil, 0, err
	}

	util.Debugf("access(%s): access %d, attr: %+v", path, accessres.Access, accessres.Attr)

	return &accessres.Attr.Attr, accessres.Access, nil
}

// ReadDirPlus get dir sub item
func (v *Target) ReadDirPlus(dir string) ([]*EntryPlus, error) {
	_, fh, err := v.Lookup(dir)
	if err != nil {
		return nil, err
	}

	return v.ReadDirPlusByFh(fh)
}

func (v *Target) ReadDirPlusByFh(fh []byte) ([]*EntryPlus, error) {
	cookie := uint64(0)
	cookieVerf := uint64(0)
	eof := false

	type ReadDirPlus3Args struct {
		rpc.Header
		FH         []byte
		Cookie     uint64
		CookieVerf uint64
		DirCount   uint32
		MaxCount   uint32
	}

	type DirListPlus3 struct {
		IsSet bool      `xdr:"union"`
		Entry EntryPlus `xdr:"unioncase=1"`
	}

	type DirListOK struct {
		DirAttrs   PostOpAttr
		CookieVerf uint64
	}

	var entries []*EntryPlus
	for !eof {
		res, err := v.call(&ReadDirPlus3Args{
			Header: rpc.Header{
				Rpcvers: 2,
				Prog:    Nfs3Prog,
				Vers:    Nfs3Vers,
				Proc:    NFSProc3ReadDirPlus,
				Cred:    v.auth,
				Verf:    rpc.AuthNull,
			},
			FH:         fh,
			Cookie:     cookie,
			CookieVerf: cookieVerf,
			DirCount:   512,
			MaxCount:   4096,
		})

		if err != nil {
			util.Debugf("readdir(%x): %s", fh, err.Error())
			return nil, err
		}

		// The dir list entries are so-called "optional-data".  We need to check
		// the Follows fields before continuing down the array.  Effectively, it's
		// an encoding used to flatten a linked list into an array where the
		// Follows field is set when the next idx has data. See
		// https://tools.ietf.org/html/rfc4506.html#section-4.19 for details.
		dirlistOK := new(DirListOK)
		if err = xdr.Read(res, dirlistOK); err != nil {
			util.Errorf("readdir failed to parse result (%x): %s", fh, err.Error())
			util.Debugf("partial dirlist: %+v", dirlistOK)
			return nil, err
		}

		for {
			var item DirListPlus3
			if err = xdr.Read(res, &item); err != nil {
				util.Errorf("readdir failed to parse directory entry, aborting")
				util.Debugf("partial dirent: %+v", item)
				return nil, err
			}

			if !item.IsSet {
				break
			}

			cookie = item.Entry.Cookie
			entries = append(entries, &item.Entry)
		}

		if err = xdr.Read(res, &eof); err != nil {
			util.Errorf("readdir failed to determine presence of more data to read, aborting")
			return nil, err
		}

		util.Debugf("No EOF for dirents so calling back for more")
		cookieVerf = dirlistOK.CookieVerf
	}

	return entries, nil
}

func (v *Target) Mkdir(path string, perm os.FileMode) ([]byte, error) {
	dir, newDir := _path.Split(path)
	_, fh, err := v.Lookup(dir)
	if err != nil {
		return nil, err
	}

	return v.MkdirByParentFh(fh, newDir, perm)
}

// Creates a directory of the given name and returns its handle
func (v *Target) MkdirByParentFh(fh []byte, name string, perm os.FileMode) ([]byte, error) {
	type MkdirArgs struct {
		rpc.Header
		Where Diropargs3
		Attrs Sattr3
	}

	type MkdirOk struct {
		FH     PostOpFH3
		Attr   PostOpAttr
		DirWcc WccData
	}

	args := &MkdirArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Mkdir,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		Where: Diropargs3{
			FH:       fh,
			Filename: name,
		},
		Attrs: Sattr3{
			Mode: SetMode{
				SetIt: true,
				Mode:  uint32(perm.Perm()),
			},
		},
	}
	res, err := v.call(args)

	if err != nil {
		util.Debugf("mkdir(%+v %s): %s", fh, name, err.Error())
		util.Debugf("mkdir args (%+v)", args)
		return nil, err
	}

	mkdirres := new(MkdirOk)
	if err := xdr.Read(res, mkdirres); err != nil {
		util.Errorf("mkdir(%+v %s) failed to parse return: %s", fh, name, err)
		util.Debugf("mkdir(%s) partial response: %+v", mkdirres)
		return nil, err
	}

	util.Debugf("mkdir(%+v %s): created successfully: %+v", fh, name, mkdirres.FH.FH)
	return mkdirres.FH.FH, nil
}

// Create a file with name the given mode
func (v *Target) CreateTruncate(path string, perm os.FileMode, size uint64) ([]byte, error) {
	_, _, newFile, fh, err := v.lookupInner(v.fh, path, false, nil)
	if err != nil {
		return nil, err
	}

	type How struct {
		// 0 : UNCHECKED (default)
		// 1 : GUARDED
		// 2 : EXCLUSIVE
		Mode uint32
		Attr Sattr3
	}
	type Create3Args struct {
		rpc.Header
		Where Diropargs3
		HW    How
	}

	type Create3Res struct {
		FH     PostOpFH3
		Attr   PostOpAttr
		DirWcc WccData
	}

	res, err := v.call(&Create3Args{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Create,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		Where: Diropargs3{
			FH:       fh,
			Filename: newFile,
		},
		HW: How{
			Attr: Sattr3{
				Mode: SetMode{
					SetIt: true,
					Mode:  uint32(perm.Perm()),
				},
				Size: SetSize{
					SetIt: true,
					Size:  size,
				},
			},
		},
	})

	if err != nil {
		util.Debugf("create(%s): %s", path, err.Error())
		return nil, err
	}

	status := new(Create3Res)
	if err = xdr.Read(res, status); err != nil {
		return nil, err
	}

	util.Debugf("create(%s): created successfully", path)
	return status.FH.FH, nil
}

// Create a file with name the given mode
func (v *Target) Create(path string, perm os.FileMode) ([]byte, error) {
	_, _, newFile, fh, err := v.lookupInner(v.fh, path, false, nil)
	if err != nil {
		return nil, err
	}

	return v.CreateByFh(fh, newFile, perm)
}

func (v *Target) GetAttr(path string) (*Fattr, []byte, error) {
	_, fh, err := v.Lookup(path)
	if err != nil {
		return nil, nil, err
	}

	fattr, err := v.GetAttrFh(fh)

	util.Debugf("getattr(%s): FH 0x%x, attr: %+v", path, fh, fattr)
	return fattr, fh, err
}

func (v *Target) GetAttrFh(fh []byte) (*Fattr, error) {
	type GetAttrArgs struct {
		rpc.Header
		FH []byte
	}

	type GetAttrOk struct {
		Attr Fattr
	}

	res, err := v.call(&GetAttrArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3GetAttr,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		FH: fh,
	})

	if err != nil {
		return nil, err
	}

	getAttrRes := new(GetAttrOk)
	if err := xdr.Read(res, getAttrRes); err != nil {
		util.Debugf("getattr partial decode: %+v", *getAttrRes)
		util.Debugf("getattr raw res: %+v", res)
		return nil, err
	}

	return &getAttrRes.Attr, nil
}

// Create a file with name the given mode
func (v *Target) CreateByFh(fh []byte, name string, perm os.FileMode) ([]byte, error) {
	type How struct {
		// 0 : UNCHECKED (default)
		// 1 : GUARDED
		// 2 : EXCLUSIVE
		Mode uint32
		Attr Sattr3
	}
	type Create3Args struct {
		rpc.Header
		Where Diropargs3
		HW    How
	}

	type Create3Res struct {
		FH     PostOpFH3
		Attr   PostOpAttr
		DirWcc WccData
	}

	res, err := v.call(&Create3Args{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Create,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		Where: Diropargs3{
			FH:       fh,
			Filename: name,
		},
		HW: How{
			Attr: Sattr3{
				Mode: SetMode{
					SetIt: true,
					Mode:  uint32(perm.Perm()),
				},
			},
		},
	})

	if err != nil {
		return nil, err
	}

	status := new(Create3Res)
	if err = xdr.Read(res, status); err != nil {
		return nil, err
	}

	util.Debugf("create(%+v %s): created successfully", fh, name)
	return status.FH.FH, nil
}

// Remove a file
func (v *Target) Remove(path string) error {
	parentDir, deleteFile := _path.Split(path)
	_, fh, err := v.Lookup(parentDir)
	if err != nil {
		return err
	}

	return v.remove(fh, deleteFile)
}

// remove the named file from the parent (fh)
func (v *Target) remove(fh []byte, deleteFile string) error {
	type RemoveArgs struct {
		rpc.Header
		Object Diropargs3
	}

	_, err := v.call(&RemoveArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Remove,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		Object: Diropargs3{
			FH:       fh,
			Filename: deleteFile,
		},
	})

	if err != nil {
		util.Debugf("remove(%s): %s", deleteFile, err.Error())
		return err
	}

	return nil
}

// RmDir removes a non-empty directory
func (v *Target) RmDir(path string) error {
	dir, deletedir := _path.Split(path)
	_, fh, err := v.Lookup(dir)
	if err != nil {
		return err
	}

	return v.rmDir(fh, deletedir)
}

// delete the named directory from the parent directory (fh)
func (v *Target) rmDir(fh []byte, name string) error {
	type RmDir3Args struct {
		rpc.Header
		Object Diropargs3
	}

	_, err := v.call(&RmDir3Args{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3RmDir,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		Object: Diropargs3{
			FH:       fh,
			Filename: name,
		},
	})

	if err != nil {
		util.Debugf("rmdir(%s): %s", name, err.Error())
		return err
	}

	util.Debugf("rmdir(%s): deleted successfully", name)
	return nil
}

func (v *Target) RemoveAll(path string) error {
	_, _, deleteDir, parentDirfh, err := v.lookupInner(v.fh, path, false, nil)
	if err != nil {
		return err
	}

	// Easy path.  This is a directory and it's empty.  If not a dir or not an
	// empty dir, this will throw an error.
	err = v.rmDir(parentDirfh, deleteDir)
	if err == nil || os.IsNotExist(err) {
		return nil
	}

	// Collect the not a dir error.
	if IsNotDirError(err) {
		return err
	}

	_, deleteDirfh, _, _, err := v.lookupInner(parentDirfh, deleteDir, true, nil)
	if err != nil {
		return err
	}

	if err = v.removeAll(deleteDirfh); err != nil {
		return err
	}

	// Delete the directory we started at.
	if err = v.rmDir(parentDirfh, deleteDir); err != nil {
		return err
	}

	return nil
}

// removeAll removes the deleteDir recursively
func (v *Target) removeAll(deleteDirfh []byte) error {

	// BFS the dir tree recursively.  If dir, recurse, then delete the dir and
	// all files.

	// This is a directory, get all of its Entries
	entries, err := v.ReadDirPlusByFh(deleteDirfh)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		// skip "." and ".."
		if entry.FileName == "." || entry.FileName == ".." {
			continue
		}

		// If directory, recurse, then nuke it.  It should be empty when we get
		// back.
		if entry.Attr.Attr.Type == NF3Dir {
			if entry.Handle.IsSet {
				if err = v.removeAll(entry.Handle.FH); err != nil {
					return err
				}
			}

			err = v.rmDir(deleteDirfh, entry.FileName)
		} else {

			// nuke all files
			err = v.remove(deleteDirfh, entry.FileName)
		}

		if err != nil {
			util.Errorf("error deleting %s: %s", entry.FileName, err.Error())
			return err
		}
	}

	return nil
}

func (v *Target) GetAttrByFh(fh []byte) (*Fattr, error) {
	type GetAttr3Args struct {
		rpc.Header
		FH []byte
	}

	type GetAttr3ResOk struct {
		Attr Fattr
	}

	res, err := v.call(&GetAttr3Args{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3GetAttr,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		FH: fh,
	})

	if err != nil {
		util.Debugf("getattr: %s", err.Error())
		return nil, err
	}

	fattr := new(Fattr)
	if err = xdr.Read(res, fattr); err != nil {
		return nil, err
	}

	return fattr, nil
}

type Guard struct {
	// 0 : FALSE
	// 1 : TRUE if the server is to verify that guard.obj_ctime matches the ctime for the object
	Check bool     `xdr:"union"`
	Ctime NFS3Time `xdr:"unioncase=1"`
}

func (v *Target) SetAttrByFh(fh []byte, fattr Sattr3) error {
	type SetAttr3Args struct {
		rpc.Header
		FH    []byte
		Fattr Sattr3
		Guard Guard
	}

	type SetAttr3ResOk struct {
		WccData WccData
	}

	res, err := v.call(&SetAttr3Args{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3SetAttr,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		FH:    fh,
		Fattr: fattr,
		Guard: Guard{
			Check: false,
		},
	})

	if err != nil {
		util.Debugf("setattr: %s", err.Error())
		return err
	}

	wccData := new(WccData)
	if err = xdr.Read(res, wccData); err != nil {
		return err
	}

	return nil
}

func (v *Target) Rename(fromPath string, toPath string) error {
	_, _, fromName, fromFh, err := v.lookupInner(v.fh, fromPath, true, nil)
	if err != nil {
		return err
	}
	if fromFh == nil {
		return fmt.Errorf("fromName cannot be a root directory")
	}
	_, _, toName, toFh, err := v.lookupInner(v.fh, toPath, false, nil)
	if err != nil {
		return err
	}
	if toFh == nil {
		return fmt.Errorf("toName cannot be a root directory")
	}
	return v.RenameByFh(fromFh, fromName, toFh, toName)
}

func (v *Target) RenameByFh(fromFh []byte, fromName string, toFh []byte, toName string) error {
	type Rename3Args struct {
		rpc.Header
		From Diropargs3
		To   Diropargs3
	}

	type Rename3Res struct {
		FromDirWcc WccData
		ToDirWcc   WccData
	}

	res, err := v.call(&Rename3Args{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Rename,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		From: Diropargs3{
			FH:       fromFh,
			Filename: fromName,
		},
		To: Diropargs3{
			FH:       toFh,
			Filename: toName,
		},
	})

	if err != nil {
		util.Debugf("rename(%+v %s): %s", fromFh, fromName, err.Error())
		return err
	}

	status := new(Rename3Res)
	if err = xdr.Read(res, status); err != nil {
		return err
	}

	util.Debugf("rename(%+v %s): successfully renamed to (%+v %s)", fromFh, fromName, toFh, toName)
	return nil
}

// Readlink reads a symbolic link and returns the target
func (v *Target) Readlink(path string) (string, error) {
	_, fh, err := v.Lookup(path)
	if err != nil {
		return "", err
	}

	_, target, err := v.readlinkFh(fh)
	return target, err
}

func (v *Target) readlinkFh(fh []byte) (*Fattr, string, error) {
	type Readlink3Arg struct {
		rpc.Header
		FH []byte
	}

	type Readlink3Ok struct {
		SymlinkAttr PostOpAttr
		Target      string
	}

	res, err := v.call(
		&Readlink3Arg{
			Header: rpc.Header{
				Rpcvers: 2,
				Prog:    Nfs3Prog,
				Vers:    Nfs3Vers,
				Proc:    NFSProc3Readlink,
				Cred:    v.auth,
				Verf:    rpc.AuthNull,
			},
			FH: fh,
		},
	)

	if err != nil {
		util.Debugf("readlink(%+v): %s", fh, err.Error())
		return nil, "", err
	}

	var readlinkRes Readlink3Ok
	if err := xdr.Read(res, &readlinkRes); err != nil {
		return nil, "", err
	}

	util.Debugf("readlink(%+v): attr: %+v, target: %s", fh, readlinkRes.SymlinkAttr.Attr, readlinkRes.Target)

	return &readlinkRes.SymlinkAttr.Attr, readlinkRes.Target, nil
}

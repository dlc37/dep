// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgtree

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/pkg/errors"
)

const (
	pathSeparator = string(filepath.Separator)

	// when walking vendor root hierarchy, ignore file system nodes of the
	// following types.
	skipModes = os.ModeDevice | os.ModeNamedPipe | os.ModeSocket | os.ModeCharDevice
)

// DigestFromPathname returns a deterministic hash of the specified file system
// node, performing a breadth-first traversal of directories. While the
// specified prefix is joined with the pathname to walk the file system, the
// prefix string is eliminated from the pathname of the nodes encounted when
// hashing the pathnames, so that the resultant hash is agnostic to the absolute
// root directory path of the nodes being checked.
//
// This function ignores any file system node named `vendor`, `.bzr`, `.git`,
// `.hg`, and `.svn`, as these are typically used as Version Control System
// (VCS) directories.
//
// Other than the `vendor` and VCS directories mentioned above, the calculated
// hash includes the pathname to every discovered file system node, whether it
// is an empty directory, a non-empty directory, empty file, non-empty file, or
// symbolic link. If a symbolic link, the referent name is included. If a
// non-empty file, the file's contents are incuded. If a non-empty directory,
// the contents of the directory are included.
//
// While filepath.Walk could have been used, that standard library function
// skips symbolic links, and for now, we want the hash to include the symbolic
// link referents.
func DigestFromPathname(prefix, pathname string) ([]byte, error) {
	// Create a single hash instance for the entire operation, rather than a new
	// hash for each node we encounter.
	h := sha256.New()

	// Initialize a work queue with the os-agnostic cleaned up pathname. Note
	// that we use `filepath.Clean` rather than `filepath.Abs`, because the hash
	// has pathnames which are relative to prefix, and there is no reason to
	// convert to absolute pathname for every invocation of this function.
	prefix = filepath.Clean(prefix) + pathSeparator
	prefixLength := len(prefix) // store length to trim off pathnames later
	pathnameQueue := []string{filepath.Join(prefix, pathname)}

	var written int64

	// As we enumerate over the queue and encounter a directory, its children
	// will be added to the work queue.
	for len(pathnameQueue) > 0 {
		// Unshift a pathname from the queue (breadth-first traversal of
		// hierarchy)
		pathname, pathnameQueue = pathnameQueue[0], pathnameQueue[1:]

		fi, err := os.Lstat(pathname)
		if err != nil {
			return nil, errors.Wrap(err, "cannot Lstat")
		}
		mode := fi.Mode()

		// Skip file system nodes we are not concerned with
		if mode&skipModes != 0 {
			continue
		}

		// Write the prefix-stripped pathname to hash because the hash is as
		// much a function of the relative names of the files and directories as
		// it is their contents. Added benefit is that even empty directories
		// and symbolic links will effect final hash value. Use
		// `filepath.ToSlash` to ensure relative pathname is os-agnostic.
		writeBytesWithNull(h, []byte(filepath.ToSlash(pathname[prefixLength:])))

		if mode&os.ModeSymlink != 0 {
			referent, err := os.Readlink(pathname)
			if err != nil {
				return nil, errors.Wrap(err, "cannot Readlink")
			}
			// Write the os-agnostic referent to the hash and proceed to the
			// next pathname in the queue.
			writeBytesWithNull(h, []byte(filepath.ToSlash(referent)))
			continue
		}

		// For both directories and regular files, we must create a file system
		// handle in order to read their contents.
		fh, err := os.Open(pathname)
		if err != nil {
			return nil, errors.Wrap(err, "cannot Open")
		}

		if fi.IsDir() {
			childrenNames, err := sortedListOfDirectoryChildrenFromFileHandle(fh)
			if err != nil {
				_ = fh.Close() // already have an error reading directory; ignore Close result.
				return nil, errors.Wrap(err, "cannot get list of directory children")
			}
			for _, childName := range childrenNames {
				switch childName {
				case ".", "..", "vendor", ".bzr", ".git", ".hg", ".svn":
					// skip
				default:
					pathnameQueue = append(pathnameQueue, filepath.Join(pathname, childName))
				}
			}
		} else {
			written, err = io.Copy(h, newLineEndingReader(fh))            // fast copy of file contents to hash
			err = errors.Wrap(err, "cannot Copy")                         // errors.Wrap only wraps non-nil, so elide guard condition
			writeBytesWithNull(h, []byte(strconv.FormatInt(written, 10))) // format file size as base 10 integer
		}

		// Close the file handle to the open directory without masking possible
		// previous error value.
		if er := fh.Close(); err == nil {
			err = errors.Wrap(er, "cannot Close")
		}
		if err != nil {
			return nil, err // early termination iff error
		}
	}

	return h.Sum(nil), nil
}

// lineEndingReader is a `io.Reader` that converts CRLF sequences to LF.
//
// Some VCS systems automatically convert LF line endings to CRLF on some OS
// platforms. This would cause the a file checked out on those platforms to have
// a different digest than the same file on platforms that do not perform this
// translation. In order to ensure file contents normalize and hash the same,
// this struct satisfies the io.Reader interface by providing a Read method that
// modifies the file's contents when it is read, translating all CRLF sequences
// to LF.
type lineEndingReader struct {
	src             io.Reader // source io.Reader from which this reads
	prevReadEndedCR bool      // used to track whether final byte of previous Read was CR
}

// newLineEndingReader returns a new lineEndingReader that reads from the
// specified source io.Reader.
func newLineEndingReader(src io.Reader) *lineEndingReader {
	return &lineEndingReader{src: src}
}

// Read consumes bytes from the structure's source io.Reader to fill the
// specified slice of bytes. It converts all CRLF byte sequences to LF, and
// handles cases where CR and LF straddle across two Read operations.
func (f *lineEndingReader) Read(buf []byte) (int, error) {
	buflen := len(buf)
	if f.prevReadEndedCR {
		// Read one less byte in case we need to insert CR in there
		buflen--
	}
	nr, er := f.src.Read(buf[:buflen])
	if nr > 0 {
		if f.prevReadEndedCR && buf[0] != '\n' {
			// Having a CRLF split across two Read operations is rare, so
			// ignoring performance impact of copying entire buffer by one
			// byte. Plus, `copy` builtin likely uses machien opcode for
			// performing the memory copy.
			copy(buf[1:nr+1], buf[:nr]) // shift data to right one byte
			buf[0] = '\r'               // insert the previous skipped CR byte at start of buf
			nr++                        // pretend we read one more byte
		}

		// Remove any CRLF sequences in buf, using `bytes.Index` because that
		// takes advantage of machine opcodes that search for byte patterns on
		// many architectures.
		var prevIndex int
		for {
			index := bytes.Index(buf[prevIndex:nr], []byte("\r\n"))
			if index == -1 {
				break
			}
			// Want to skip index byte, where the CR is.
			copy(buf[prevIndex+index:nr-1], buf[prevIndex+index+1:nr])
			nr--
			prevIndex = index
		}

		// When final byte from a read operation is CR, do not emit it until
		// ensure first byte on next read is not LF.
		if f.prevReadEndedCR = buf[nr-1] == '\r'; f.prevReadEndedCR {
			nr-- // pretend byte was never read from source
		}
	} else if f.prevReadEndedCR {
		// Reading from source returned nothing, but this struct is sitting on a
		// trailing CR from previous Read, so let's give it to client now.
		buf[0] = '\r'
		nr = 1
		er = nil
		f.prevReadEndedCR = false // prevent infinite loop
	}
	return nr, er
}

// writeBytesWithNull appends the specified data to the specified hash, followed by
// the NULL byte, in order to make accidental hash collisions less likely.
func writeBytesWithNull(h hash.Hash, data []byte) {
	// Ignore return values from writing to the hash, because hash write always
	// returns nil error.
	_, _ = h.Write(append(data, 0))
}

// VendorStatus represents one of a handful of possible statuses of a particular
// subdirectory under vendor.
type VendorStatus uint8

const (
	// NotInLock is used when a file system node exists for which there is no
	// corresponding dependency in the lock file.
	NotInLock VendorStatus = iota

	// NotInTree is used when a lock file dependency exists for which there is
	// no corresponding file system node.
	NotInTree

	// NoMismatch is used when the digest for a dependency listed in the
	// lockfile matches what is calculated from the file system.
	NoMismatch

	// EmptyDigestInLock is used when the digest for a dependency listed in the
	// lock file is the empty string. NOTE: Seems like a special case of
	// DigestMismatchInLock.
	EmptyDigestInLock

	// DigestMismatchInLock is used when the digest for a dependency listed in
	// the lock file does not match what is calculated from the file system.
	DigestMismatchInLock
)

func (ls VendorStatus) String() string {
	switch ls {
	case NotInTree:
		return "not in tree"
	case NotInLock:
		return "not in lock"
	case NoMismatch:
		return "match"
	case EmptyDigestInLock:
		return "empty digest in lock"
	case DigestMismatchInLock:
		return "mismatch"
	}
	return "unknown"
}

type fsnode struct {
	pathname             string
	isRequiredAncestor   bool
	myIndex, parentIndex int
}

func (n fsnode) String() string {
	return fmt.Sprintf("[%d:%d %q %t]", n.myIndex, n.parentIndex, n.pathname, n.isRequiredAncestor)
}

// sortedListOfDirectoryChildrenFromPathname returns a lexicographical sorted
// list of child nodes for the specified directory.
func sortedListOfDirectoryChildrenFromPathname(pathname string) ([]string, error) {
	fh, err := os.Open(pathname)
	if err != nil {
		return nil, errors.Wrap(err, "cannot Open")
	}

	childrenNames, err := sortedListOfDirectoryChildrenFromFileHandle(fh)

	// Close the file handle to the open directory without masking possible
	// previous error value.
	if er := fh.Close(); err == nil {
		err = errors.Wrap(er, "cannot Close")
	}

	return childrenNames, err
}

// sortedListOfDirectoryChildrenFromPathname returns a lexicographical sorted
// list of child nodes for the specified open file handle to a directory. This
// function is written once to avoid writing the logic in two places.
func sortedListOfDirectoryChildrenFromFileHandle(fh *os.File) ([]string, error) {
	childrenNames, err := fh.Readdirnames(0) // 0: read names of all children
	if err != nil {
		return nil, errors.Wrap(err, "cannot Readdirnames")
	}
	if len(childrenNames) > 0 {
		sort.Strings(childrenNames)
	}
	return childrenNames, nil
}

// VerifyDepTree verifies dependency tree according to expected digest sums, and
// returns an associative array of file system nodes and their respective vendor
// status, in accordance with the provided expected digest sums parameter.
func VerifyDepTree(vendorPathname string, wantSums map[string][]byte) (map[string]VendorStatus, error) {
	// NOTE: Ensure top level pathname is a directory
	fi, err := os.Stat(vendorPathname)
	if err != nil {
		return nil, errors.Wrap(err, "cannot Stat")
	}
	if !fi.IsDir() {
		return nil, errors.Errorf("cannot verify non directory: %q", vendorPathname)
	}

	vendorPathname = filepath.Clean(vendorPathname) + pathSeparator
	prefixLength := len(vendorPathname)

	var otherNode *fsnode
	currentNode := &fsnode{pathname: vendorPathname, parentIndex: -1, isRequiredAncestor: true}
	queue := []*fsnode{currentNode} // queue of directories that must be inspected

	// In order to identify all file system nodes that are not in the lock file,
	// represented by the specified expected sums parameter, and in order to
	// only report the top level of a subdirectory of file system nodes, rather
	// than every node internal to them, we will create a tree of nodes stored
	// in a slice.  We do this because we do not know at what level a project
	// exists at. Some projects are fewer than and some projects more than the
	// typical three layer subdirectory under the vendor root directory.
	//
	// For a following few examples, assume the below vendor root directory:
	//
	// github.com/alice/alice1/a1.go
	// github.com/alice/alice2/a2.go
	// github.com/bob/bob1/b1.go
	// github.com/bob/bob2/b2.go
	// launghpad.net/nifty/n1.go
	//
	// 1) If only the `alice1` and `alice2` projects were in the lock file, we'd
	// prefer the output to state that `github.com/bob` is `NotInLock`.
	//
	// 2) If `alice1`, `alice2`, and `bob1` were in the lock file, we'd want to
	// report `github.com/bob/bob2` as `NotInLock`.
	//
	// 3) If none of `alice1`, `alice2`, `bob1`, or `bob2` were in the lock
	// file, the entire `github.com` directory would be reported as `NotInLock`.
	//
	// Each node in our tree has the slice index of its parent node, so once we
	// can categorically state a particular directory is required because it is
	// in the lock file, we can mark all of its ancestors as also being
	// required. Then, when we finish walking the directory hierarchy, any nodes
	// which are not required but have a required parent will be marked as
	// `NotInLock`.
	nodes := []*fsnode{currentNode}

	// Mark directories of expected projects as required. When the respective
	// project is found in the vendor root hierarchy, its status will be updated
	// to reflect whether its digest is empty, or whether or not it matches the
	// expected digest.
	status := make(map[string]VendorStatus)
	for pathname := range wantSums {
		status[pathname] = NotInTree
	}

	for len(queue) > 0 {
		// pop node from the queue (depth first traversal, reverse lexicographical order)
		lq1 := len(queue) - 1
		currentNode, queue = queue[lq1], queue[:lq1]

		// log.Printf("NODE: %s", currentNode)
		short := currentNode.pathname[prefixLength:] // chop off the vendor root prefix, including the path separator
		if expectedSum, ok := wantSums[short]; ok {
			ls := EmptyDigestInLock
			if len(expectedSum) > 0 {
				ls = NoMismatch
				projectSum, err := DigestFromPathname(vendorPathname, short)
				if err != nil {
					return nil, errors.Wrap(err, "cannot compute dependency hash")
				}
				if !bytes.Equal(projectSum, expectedSum) {
					ls = DigestMismatchInLock
				}
			}
			status[short] = ls

			// NOTE: Mark current nodes and all parents: required.
			for pni := currentNode.myIndex; pni != -1; pni = otherNode.parentIndex {
				otherNode = nodes[pni]
				otherNode.isRequiredAncestor = true
				// log.Printf("parent node: %s", otherNode)
			}

			continue // do not need to process directory's contents
		}

		childrenNames, err := sortedListOfDirectoryChildrenFromPathname(currentNode.pathname)
		if err != nil {
			return nil, errors.Wrap(err, "cannot get sorted list of directory children")
		}
		for _, childName := range childrenNames {
			switch childName {
			case ".", "..", "vendor", ".bzr", ".git", ".hg", ".svn":
				// skip
			default:
				childPathname := filepath.Join(currentNode.pathname, childName)
				otherNode = &fsnode{pathname: childPathname, myIndex: len(nodes), parentIndex: currentNode.myIndex}

				fi, err := os.Stat(childPathname)
				if err != nil {
					return nil, errors.Wrap(err, "cannot Stat")
				}
				// Skip non-interesting file system nodes
				mode := fi.Mode()
				if mode&skipModes != 0 || mode&os.ModeSymlink != 0 {
					// log.Printf("DEBUG: skipping: %v; %q", mode, currentNode.pathname)
					continue
				}

				nodes = append(nodes, otherNode)
				if fi.IsDir() {
					queue = append(queue, otherNode)
				}
			}
		}

		if err != nil {
			return nil, err // early termination iff error
		}
	}

	// Ignoring first node in the list, walk nodes from end to
	// beginning. Whenever a node is not required, but its parent is required,
	// then that node and all under it ought to be marked as `NotInLock`.
	for i := len(nodes) - 1; i > 0; i-- {
		currentNode = nodes[i]
		if !currentNode.isRequiredAncestor && nodes[currentNode.parentIndex].isRequiredAncestor {
			status[currentNode.pathname[prefixLength:]] = NotInLock
		}
	}

	return status, nil
}

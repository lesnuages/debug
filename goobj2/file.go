// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package goobj implements reading of Go object files and archives.

// This file is a modified version of cmd/internal/goobj/readnew.go

package goobj2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Binject/debug/goobj2/internal/goobj2"
	"github.com/Binject/debug/goobj2/internal/objabi"
)

const (
	archiveHeaderLen = 60
	initSymName      = `""..inittask`
	objHeaderLen     = 80
)

// A Package is a parsed Go object file or archive defining a Go package.
type Package struct {
	ArchiveHeaders []ArchiveHeader
	Header         goobj2.Header
	ImportPath     string
	Imports        []goobj2.ImportedPkg // packages imported by this package
	Packages       []string
	DWARFFileList  []string // List of files for the DWARF .debug_lines section
	SymDefs        []*Sym
	NonPkgSymDefs  []*Sym
	NonPkgSymRefs  []*Sym
	SymRefs        []SymRef
	OS             string
	Arch           string // architecture

	textSyms textSyms
	initSym  specialSym

	symMap map[int]*Sym
}

type ArchiveHeader struct {
	Name string
	Date string
	UID  string
	GID  string
	Mode string
	Size int64
	Data []byte
}

// A Sym is a named symbol in an object file.
type Sym struct {
	Name   string
	ABI    uint16
	Kind   objabi.SymKind // kind of symbol
	Flag   uint8
	Size   uint32 // size of corresponding data
	Align  uint32
	Type   *SymRef // symbol for Go type information
	Data   []byte  // memory image of symbol
	Reloc  []Reloc // relocations to apply to Data
	Func   *Func   // additional data for functions
	symRef goobj2.SymRef
}

type SymRef struct {
	Name string
	goobj2.SymRef
}

// A Reloc describes a relocation applied to a memory image to refer
// to an address within a particular symbol.
type Reloc struct {
	Name string
	// The bytes at [Offset, Offset+Size) within the containing Sym
	// should be updated to refer to the address Add bytes after the start
	// of the symbol Sym.
	Offset int64
	Size   int64
	Sym    goobj2.SymRef
	Add    int64

	// The Type records the form of address expected in the bytes
	// described by the previous fields: absolute, PC-relative, and so on.
	// TODO(rsc): The interpretation of Type is not exposed by this package.
	Type objabi.RelocType
}

// Func contains additional per-symbol information specific to functions.
type Func struct {
	Args     int64      // size in bytes of argument frame: inputs and outputs
	Frame    int64      // size in bytes of local variable frame
	PCSP     []byte     // PC → SP offset map
	PCFile   []byte     // PC → file number map (index into File)
	PCLine   []byte     // PC → line number map
	PCInline []byte     // PC → inline tree index map
	PCData   [][]byte   // PC → runtime support data map
	FuncData []FuncData // non-PC-specific runtime support data
	File     []SymRef   // paths indexed by PCFile
	InlTree  []*InlinedCall

	FuncInfo        *SymRef
	DwarfInfo       *SymRef
	DwarfLoc        *SymRef
	DwarfRanges     *SymRef
	DwarfDebugLines *SymRef

	dataSymIdx int
}

// TODO: Add PCData []byte and PCDataIter (similar to liblink).

// A FuncData is a single function-specific data value.
type FuncData struct {
	Sym    *SymRef // symbol holding data
	Offset uint32  // offset into symbol for funcdata pointer
}

// An InlinedCall is a node in an InlTree.
// See cmd/internal/obj.InlTree for details.
type InlinedCall struct {
	Parent   int32
	File     SymRef
	Line     int32
	Func     SymRef
	ParentPC int32
}

type textSyms []specialSym

func (t textSyms) Len() int {
	return len(t)
}

func (t textSyms) Less(i, j int) bool {
	return t[i].strOff < t[j].strOff
}

func (t textSyms) Swap(i, j int) {
	var temp specialSym

	temp = t[i]
	t[i] = t[j]
	t[j] = temp
}

type specialSym struct {
	strOff uint32
	sym    *Sym
}

var (
	archiveHeader = []byte("!<arch>\n")
	archiveMagic  = []byte("`\n")
	goobjHeader   = []byte("go objec") // truncated to size of archiveHeader

	archivePathPrefix = filepath.Join("$GOROOT", "pkg")

	errCorruptArchive   = errors.New("corrupt archive")
	errTruncatedArchive = errors.New("truncated archive")
	errCorruptObject    = errors.New("corrupt object file")
	errNotObject        = errors.New("unrecognized object file format")
)

// An objReader is an object file reader.
type objReader struct {
	p         *Package
	b         *bufio.Reader
	f         *os.File
	err       error
	offset    int64
	limit     int64
	tmp       [256]byte
	pkgprefix string
	objStart  int64
}

// init initializes r to read package p from f.
func (r *objReader) init(f *os.File, p *Package) {
	r.f = f
	r.p = p
	r.offset, _ = f.Seek(0, io.SeekCurrent)
	r.limit, _ = f.Seek(0, io.SeekEnd)
	f.Seek(r.offset, io.SeekStart)
	r.b = bufio.NewReader(f)
	if p != nil {
		r.pkgprefix = objabi.PathToPrefix(p.ImportPath) + "."
	}
}

// error records that an error occurred.
// It returns only the first error, so that an error
// caused by an earlier error does not discard information
// about the earlier error.
func (r *objReader) error(err error) error {
	if r.err == nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.err = err
	}
	// panic("corrupt") // useful for debugging
	return r.err
}

// peek returns the next n bytes without advancing the reader.
func (r *objReader) peek(n int) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.offset >= r.limit {
		r.error(io.ErrUnexpectedEOF)
		return nil, r.err
	}
	b, err := r.b.Peek(n)
	if err != nil {
		if err != bufio.ErrBufferFull {
			r.error(err)
		}
	}
	return b, err
}

// readByte reads and returns a byte from the input file.
// On I/O error or EOF, it records the error but returns byte 0.
// A sequence of 0 bytes will eventually terminate any
// parsing state in the object file. In particular, it ends the
// reading of a varint.
func (r *objReader) readByte() byte {
	if r.err != nil {
		return 0
	}
	if r.offset >= r.limit {
		r.error(io.ErrUnexpectedEOF)
		return 0
	}
	b, err := r.b.ReadByte()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.error(err)
		b = 0
	} else {
		r.offset++
	}
	return b
}

// read reads exactly len(b) bytes from the input file.
// If an error occurs, read returns the error but also
// records it, so it is safe for callers to ignore the result
// as long as delaying the report is not a problem.
func (r *objReader) readFull(b []byte) error {
	if r.err != nil {
		return r.err
	}
	if r.offset+int64(len(b)) > r.limit {
		return r.error(io.ErrUnexpectedEOF)
	}
	n, err := io.ReadFull(r.b, b)
	r.offset += int64(n)
	if err != nil {
		return r.error(err)
	}
	return nil
}

// readInt reads a zigzag varint from the input file.
func (r *objReader) readInt() int64 {
	var u uint64

	for shift := uint(0); ; shift += 7 {
		if shift >= 64 {
			r.error(errCorruptObject)
			return 0
		}
		c := r.readByte()
		u |= uint64(c&0x7F) << shift
		if c&0x80 == 0 {
			break
		}
	}

	return int64(u>>1) ^ (int64(u) << 63 >> 63)
}

// skip skips n bytes in the input.
func (r *objReader) skip(n int64) {
	if n < 0 {
		r.error(fmt.Errorf("debug/goobj: internal error: misuse of skip"))
	}
	if n < int64(len(r.tmp)) {
		// Since the data is so small, a just reading from the buffered
		// reader is better than flushing the buffer and seeking.
		r.readFull(r.tmp[:n])
	} else if n <= int64(r.b.Buffered()) {
		// Even though the data is not small, it has already been read.
		// Advance the buffer instead of seeking.
		for n > int64(len(r.tmp)) {
			r.readFull(r.tmp[:])
			n -= int64(len(r.tmp))
		}
		r.readFull(r.tmp[:n])
	} else {
		// Seek, giving up buffered data.
		_, err := r.f.Seek(r.offset+n, io.SeekStart)
		if err != nil {
			r.error(err)
		}
		r.offset += n
		r.b.Reset(r.f)
	}
}

// Parse parses an object file or archive from f,
// assuming that its import path is pkgpath.
func Parse(objPath, pkgPath string, importCfg []byte) (*Package, error) {
	p := new(Package)
	p.symMap = make(map[int]*Sym)
	p.ImportPath = pkgPath

	if _, err := parse(objPath, p, importCfg, false); err != nil {
		return nil, err
	}

	return p, nil
}

func parse(objPath string, p *Package, importCfg []byte, returnReader bool) (rr *goobj2.Reader, err error) {
	f, openErr := os.Open(objPath)
	if err != nil {
		return nil, openErr
	}
	defer func() {
		closeErr := f.Close()
		if err != nil && closeErr != nil {
			err = closeErr
		}
	}()

	var rd objReader
	rd.init(f, p)
	err = rd.readFull(rd.tmp[:8])
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	switch {
	default:
		return nil, errNotObject
	case bytes.Equal(rd.tmp[:8], archiveHeader):
		rr, err = rd.parseArchive(importCfg, returnReader)
		if err != nil {
			return nil, err
		}
	case bytes.Equal(rd.tmp[:8], goobjHeader):
		rr, _, err = rd.parseObject(goobjHeader, importCfg, returnReader)
		if err != nil {
			return nil, err
		}
	}

	return rr, nil
}

// trimSpace removes trailing spaces from b and returns the corresponding string.
// This effectively parses the form used in archive headers.
func trimSpace(b []byte) string {
	return string(bytes.TrimRight(b, " "))
}

// parseArchive parses a Unix archive of Go object files.
func (r *objReader) parseArchive(importCfg []byte, returnReader bool) (*goobj2.Reader, error) {
	for r.offset < r.limit {
		if err := r.readFull(r.tmp[:archiveHeaderLen]); err != nil {
			return nil, err
		}
		data := r.tmp[:archiveHeaderLen]

		// Each file is preceded by this text header (slice indices in first column):
		//	 0:16	name
		//	16:28 date
		//	28:34 uid
		//	34:40 gid
		//	40:48 mode
		//	48:58 size
		//	58:60 magic - `\n
		// The fields are space-padded on the right.
		// The size is in decimal.
		// The file data - size bytes - follows the header.
		// Headers are 2-byte aligned, so if size is odd, an extra padding
		// byte sits between the file data and the next header.
		// The file data that follows is padded to an even number of bytes:
		// if size is odd, an extra padding byte is inserted between the next header.
		if len(data) < archiveHeaderLen {
			return nil, errTruncatedArchive
		}
		if !bytes.Equal(data[58:60], archiveMagic) {
			return nil, errCorruptArchive
		}

		var ar ArchiveHeader
		ar.Name = trimSpace(data[0:16])
		ar.Date = trimSpace(data[16:28])
		ar.UID = trimSpace(data[28:34])
		ar.GID = trimSpace(data[34:40])
		ar.Mode = trimSpace(data[40:48])
		size, err := strconv.ParseInt(trimSpace(data[48:58]), 10, 64)
		if err != nil {
			return nil, errCorruptArchive
		}

		data = data[archiveHeaderLen:]
		fsize := size + size&1
		if fsize < 0 || fsize < size {
			return nil, errCorruptArchive
		}
		ar.Size = size

		switch ar.Name {
		case "__.PKGDEF":
			ar.Data = make([]byte, size)
			if err := r.readFull(ar.Data); err != nil {
				return nil, err
			}
			if fsize != size {
				ar.Data = append(ar.Data, 0x00)
			}

		default:
			oldLimit := r.limit
			r.limit = r.offset + size

			p, err := r.peek(8)
			if err != nil {
				return nil, err
			}
			if bytes.Equal(p, goobjHeader) {
				rr, data, err := r.parseObject(nil, importCfg, returnReader)
				if err != nil {
					return nil, fmt.Errorf("parsing archive member %q: %v", ar.Name, err)
				}
				if returnReader {
					return rr, nil
				}
				ar.Data = data
			}

			r.skip(r.limit - r.offset)
			r.limit = oldLimit
		}
		if size&1 != 0 {
			r.skip(1)
		}

		if r.p != nil {
			r.p.ArchiveHeaders = append(r.p.ArchiveHeaders, ar)
		}
	}

	return nil, nil
}

// parseObject parses a single Go object file.
// The prefix is the bytes already read from the file,
// typically in order to detect that this is an object file.
// The object file consists of a textual header ending in "\n!\n"
// and then the part we want to parse begins.
// The format of that part is defined in a comment at the top
// of src/liblink/objfile.c.
func (r *objReader) parseObject(prefix, importCfg []byte, returnReader bool) (*goobj2.Reader, []byte, error) {
	h := make([]byte, 0, 256)
	h = append(h, prefix...)
	var c1, c2, c3 byte
	for {
		c1, c2, c3 = c2, c3, r.readByte()
		h = append(h, c3)
		// The new export format can contain 0 bytes.
		// Don't consider them errors, only look for r.err != nil.
		if r.err != nil {
			return nil, nil, errCorruptObject
		}
		if c1 == '\n' && c2 == '!' && c3 == '\n' {
			break
		}
	}

	hs := strings.Fields(string(h))
	if len(hs) >= 4 && !returnReader {
		r.p.OS = hs[2]
		r.p.Arch = hs[3]
	}

	p, err := r.peek(8)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(p, []byte(goobj2.Magic)) {
		return nil, nil, errNotObject
	}

	r.objStart = r.offset
	length := r.limit - r.offset
	objbytes := make([]byte, length)
	r.readFull(objbytes)
	rr := goobj2.NewReaderFromBytes(objbytes, false)
	if rr == nil {
		return nil, nil, errCorruptObject
	}
	if returnReader {
		return rr, nil, nil
	}

	// Header
	r.p.Header = rr.Header()

	// Imports
	for _, p := range rr.Autolib() {
		r.p.Imports = append(r.p.Imports, p)
	}

	// Referenced packages
	r.p.Packages = rr.Pkglist()

	// Dwarf file table
	r.p.DWARFFileList = make([]string, rr.NDwarfFile())
	for i := 0; i < len(r.p.DWARFFileList); i++ {
		r.p.DWARFFileList[i] = rr.DwarfFile(i)
	}

	// Name of referenced indexed symbols.
	nrefName := rr.NRefName()
	refNames := make(map[goobj2.SymRef]string, nrefName)
	r.p.SymRefs = make([]SymRef, 0, nrefName)
	for i := 0; i < nrefName; i++ {
		rn := rr.RefName(i)
		sym, name := rn.Sym(), rn.Name(rr)
		refNames[sym] = name
		r.p.SymRefs = append(r.p.SymRefs, SymRef{name, sym})
	}

	resolveSymRefName := func(s goobj2.SymRef) string {
		var i int
		switch p := s.PkgIdx; p {
		case goobj2.PkgIdxInvalid:
			if s.SymIdx != 0 {
				panic("bad sym ref")
			}
			return ""
		case goobj2.PkgIdxNone:
			i = int(s.SymIdx) + rr.NSym()
		case goobj2.PkgIdxBuiltin:
			name, _ := goobj2.BuiltinName(int(s.SymIdx))
			return name
		case goobj2.PkgIdxSelf:
			i = int(s.SymIdx)
		default:
			return refNames[s]
		}
		sym := rr.Sym(i)
		return sym.Name(rr)
	}

	// Symbols
	pcdataBase := rr.PcdataBase()
	ndef := rr.NSym() + rr.NNonpkgdef()
	var inlFuncsToResolve []*InlinedCall

	parseSym := func(i, j int, symDefs []*Sym) {
		osym := rr.Sym(i)

		sym := &Sym{
			Name:  osym.Name(rr),
			ABI:   osym.ABI(),
			Kind:  objabi.SymKind(osym.Type()),
			Flag:  osym.Flag(),
			Size:  osym.Siz(),
			Align: osym.Align(),
		}
		symDefs[j] = sym
		r.p.symMap[i] = sym

		if i >= ndef {
			return // not a defined symbol from here
		}

		if sym.Kind == objabi.STEXT {
			r.p.textSyms = append(r.p.textSyms, specialSym{sym: sym})
		}

		// Symbol data
		sym.Data = rr.Data(i)

		// Reloc
		relocs := rr.Relocs(i)
		sym.Reloc = make([]Reloc, len(relocs))
		for j := range relocs {
			rel := &relocs[j]
			s := rel.Sym()
			sym.Reloc[j] = Reloc{
				Name:   resolveSymRefName(s),
				Offset: int64(rel.Off()),
				Size:   int64(rel.Siz()),
				Type:   objabi.RelocType(rel.Type()),
				Add:    rel.Add(),
				Sym:    s,
			}
		}

		// Aux symbol info
		isym := -1
		funcdata := make([]*SymRef, 0, 4)
		var funcInfo, dinfo, dloc, dranges, dlines *SymRef
		auxs := rr.Auxs(i)
		for j := range auxs {
			a := &auxs[j]
			switch a.Type() {
			case goobj2.AuxGotype:
				s := a.Sym()
				sym.Type = &SymRef{resolveSymRefName(s), s}
			case goobj2.AuxFuncInfo:
				sr := a.Sym()
				if sr.PkgIdx != goobj2.PkgIdxSelf {
					panic("funcinfo symbol not defined in current package")
				}
				funcInfo = &SymRef{resolveSymRefName(sr), sr}
				isym = int(a.Sym().SymIdx)
			case goobj2.AuxFuncdata:
				sr := a.Sym()
				funcdata = append(funcdata, &SymRef{resolveSymRefName(sr), sr})
			case goobj2.AuxDwarfInfo:
				sr := a.Sym()
				dinfo = &SymRef{resolveSymRefName(sr), sr}
			case goobj2.AuxDwarfLoc:
				sr := a.Sym()
				dloc = &SymRef{resolveSymRefName(sr), sr}
			case goobj2.AuxDwarfRanges:
				sr := a.Sym()
				dranges = &SymRef{resolveSymRefName(sr), sr}
			case goobj2.AuxDwarfLines:
				sr := a.Sym()
				dlines = &SymRef{resolveSymRefName(sr), sr}
			default:
				panic("unknown aux type")
			}
		}

		// Symbol Info
		if isym == -1 {
			return
		}
		b := rr.Data(isym)
		info := goobj2.FuncInfo{}
		info.Read(b)

		info.Pcdata = append(info.Pcdata, info.PcdataEnd) // for the ease of knowing where it ends
		f := &Func{
			Args:       int64(info.Args),
			Frame:      int64(info.Locals),
			PCSP:       rr.BytesAt(pcdataBase+info.Pcsp, int(info.Pcfile-info.Pcsp)),
			PCFile:     rr.BytesAt(pcdataBase+info.Pcfile, int(info.Pcline-info.Pcfile)),
			PCLine:     rr.BytesAt(pcdataBase+info.Pcline, int(info.Pcinline-info.Pcline)),
			PCInline:   rr.BytesAt(pcdataBase+info.Pcinline, int(info.Pcdata[0]-info.Pcinline)),
			PCData:     make([][]byte, len(info.Pcdata)-1), // -1 as we appended one above
			FuncData:   make([]FuncData, len(info.Funcdataoff)),
			File:       make([]SymRef, len(info.File)),
			InlTree:    make([]*InlinedCall, len(info.InlTree)),
			FuncInfo:   funcInfo,
			dataSymIdx: isym,
		}
		sym.Func = f
		for k := range f.PCData {
			f.PCData[k] = rr.BytesAt(pcdataBase+info.Pcdata[k], int(info.Pcdata[k+1]-info.Pcdata[k]))
		}
		for k := range f.FuncData {
			f.FuncData[k] = FuncData{funcdata[k], info.Funcdataoff[k]}
		}
		for k := range f.File {
			f.File[k] = SymRef{resolveSymRefName(info.File[k]), info.File[k]}
		}
		for k := range f.InlTree {
			inl := &info.InlTree[k]
			f.InlTree[k] = &InlinedCall{
				Parent:   inl.Parent,
				File:     SymRef{resolveSymRefName(inl.File), inl.File},
				Line:     inl.Line,
				Func:     SymRef{resolveSymRefName(inl.Func), inl.Func},
				ParentPC: inl.ParentPC,
			}

			if f.InlTree[k].Func.Name == "" {
				inlFuncsToResolve = append(inlFuncsToResolve, f.InlTree[k])
			}
		}
		if dinfo != nil {
			f.DwarfInfo = dinfo
		}
		if dloc != nil {
			f.DwarfLoc = dloc
		}
		if dranges != nil {
			f.DwarfRanges = dranges
		}
		if dlines != nil {
			f.DwarfDebugLines = dlines
		}
	}

	// Symbol definitions
	nsymDefs := rr.NSym()
	r.p.SymDefs = make([]*Sym, nsymDefs)
	realInitSymName := initSymName
	if r.p.ImportPath != "" {
		realInitSymName = strings.Replace(initSymName, `""`, r.p.ImportPath, 1)
	}
	for i := 0; i < nsymDefs; i++ {
		parseSym(i, i, r.p.SymDefs)
		if r.p.SymDefs[i].Name == realInitSymName {
			r.p.initSym = specialSym{sym: r.p.SymDefs[i]}
		}
	}

	// Non-pkg symbol definitions
	nNonPkgDefs := rr.NNonpkgdef()
	r.p.NonPkgSymDefs = make([]*Sym, nNonPkgDefs)
	parsedSyms := nsymDefs
	for i := 0; i < nNonPkgDefs; i++ {
		parseSym(i+parsedSyms, i, r.p.NonPkgSymDefs)
	}

	// Non-pkg symbol references
	nNonPkgRefs := rr.NNonpkgref()
	r.p.NonPkgSymRefs = make([]*Sym, nNonPkgRefs)
	parsedSyms += nNonPkgDefs
	for i := 0; i < nNonPkgRefs; i++ {
		parseSym(i+parsedSyms, i, r.p.NonPkgSymRefs)
	}

	// Symbol references were already parsed above

	// Sort text symbols
	if err := r.configureSpecialSyms(objbytes); err != nil {
		return nil, nil, err
	}
	sort.Sort(r.p.textSyms)

	// Resolve missing inlined function names
	if len(inlFuncsToResolve) == 0 {
		return nil, h, nil
	}

	var importMap map[string]string
	if len(importCfg) != 0 {
		importMap, err = parseImportCfg(importCfg)
		if err != nil {
			return nil, nil, err
		}
	}

	objReaders := make([]*goobj2.Reader, len(r.p.Packages)-1)
	for _, inl := range inlFuncsToResolve {
		if pkgIdx := inl.Func.PkgIdx; objReaders[pkgIdx-1] == nil {
			pkgName := r.p.Packages[pkgIdx]
			archivePath, err := getArchivePath(pkgName, importMap)
			if err != nil {
				return nil, nil, fmt.Errorf("error resolving path of archive: %v", err)
			}
			rr, err := parse(archivePath, nil, nil, true)
			if err != nil {
				return nil, nil, fmt.Errorf("error parsing stdlib archive: %v", err)
			}
			objReaders[pkgIdx-1] = rr
		}

		rr := objReaders[inl.Func.PkgIdx-1]
		inl.Func.Name = rr.Sym(int(inl.Func.SymIdx)).Name(rr)
	}

	return nil, h, nil
}

func getArchivePath(pkg string, importMap map[string]string) (string, error) {
	if importMap != nil {
		path, ok := importMap[pkg]
		if !ok {
			return "", fmt.Errorf("could not find package %s in importcfg", pkg)
		}
		return path, nil
	}

	cmd := exec.Command("go", "list", "-export", "-f", "{{.Export}}", pkg)
	path, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(path)), nil
}

func (r *objReader) configureSpecialSyms(objBytes []byte) error {
	stringTable := objBytes[objHeaderLen:r.p.Header.Offsets[goobj2.BlkAutolib]]

	pkgName := `""`
	if r.p.ImportPath != "" {
		pkgName = r.p.ImportPath
	}
	prefixes := []string{"go.info.", "go.string.", "type.", "runtime.", "gclocals·", "go.itablink.", "go.itab.", "gofile..", "go.interface", "go.builtin.", "go.cuinfo.packagename.", "internal/", pkgName + "."}
	for _, pkg := range r.p.Packages {
		prefixes = append(prefixes, pkg+".")
	}

	strTableOff := func(symName string) int {
		start := 0
		for {
			off := bytes.Index(stringTable[start:], []byte(symName))
			if off == -1 {
				return -1
			}

			// check to make sure the string we found isn't actually
			// a substring of another symbol's name
			newStart := start + off + len(symName)
			if !isEndOfStr(stringTable[newStart:], prefixes) {
				start = newStart
				continue
			}

			return off + start
		}
	}

	// sort the symbols in the TEXT region by when their name appears in the
	// string table.
	// TODO: find better way to order/sort text syms
	for i, textSym := range r.p.textSyms {
		off := strTableOff(textSym.sym.Name)
		if off == -1 {
			return fmt.Errorf("symbol not found in string table: %s", textSym.sym.Name)
		}

		r.p.textSyms[i].strOff = uint32(off) + objHeaderLen
	}

	// find the offset in the string table of the init symbol.
	// TODO: find better way to know when to write init symbol
	if r.p.initSym.sym != nil {
		off := strTableOff(r.p.initSym.sym.Name)
		if off == -1 {
			return fmt.Errorf("symbol not found in string table: %s", r.p.initSym.sym.Name)
		}
		r.p.initSym.strOff = uint32(off) + objHeaderLen
	}

	return nil
}

// TODO: detect if inside string
func isEndOfStr(stringTable []byte, prefixes []string) bool {
	if stringTable[0] == 46 { // '.'
		return false
	}

	// check if there's some weird character at the end of a string.
	// If there is, the string is not finished. ex 'runtime.nilinterequalÂ·f'
	if !isSymChar(stringTable[0]) {
		return false
	}

	for _, prefix := range prefixes {
		if string(stringTable[:len(prefix)]) == prefix {
			return true
		}
	}

	return false
}

func isSymChar(c byte) bool {
	return (c >= 40 && c <= 42) || // '(', ')', '*'
		(c >= 47 && c <= 57) || // '/' and 0..9
		(c >= 65 && c <= 90) || // A..Z
		c == 91 || c == 93 || c == 95 || // '[', ']', '_'
		(c >= 97 && c <= 122) // a..z
}

func parseImportCfg(importCfg []byte) (map[string]string, error) {
	lines := bytes.Count(importCfg, []byte("\n"))
	if lines == -1 {
		return nil, errors.New("error parsing importcfg: could not find any newlines")
	}

	importMap := make(map[string]string, lines)
	r := bufio.NewScanner(bytes.NewReader(importCfg))
	r.Split(bufio.ScanRunes)

	var sb strings.Builder
	var temp string
	var pkgstr bool
	for r.Scan() {
		t := r.Text()
		switch t {
		case " ":
			pkgstr = true
			continue
		case "=":
			temp = sb.String()
			sb.Reset()
		case "\n", "\r":
			importMap[temp] = sb.String()
			sb.Reset()
			pkgstr = false
		default:
			if pkgstr {
				sb.WriteString(t)
			}
		}
	}

	if err := r.Err(); err != nil {
		return nil, err
	}

	return importMap, nil
}

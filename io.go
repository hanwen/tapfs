package tapfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"log"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/grit/gritfs"
)

type fsAPI interface {
	fs.InodeEmbedder

	hashForPath(path string) (FileInfo, error)
	fromActionCache(*ActionCacheValue) error
}

// CommandServer serves RPC calls
type CommandServer struct {
	tapFSData  *tapFSData
	root       fsAPI
	mountPoint string
	FSServer   *fuse.Server
	cas        CAS
	ac         ActionCache

	Debug    bool
	mu       sync.Mutex
	debugLog io.WriteCloser
	listener net.Listener
}

func (c *CommandServer) Close() error {
	return c.listener.Close()
}
func (c *CommandServer) Wait() {
	c.FSServer.Wait()
}

const socketName = ".tapfs"

func NewCommandServer(root fs.InodeEmbedder, mntDir string, cas CAS, ac ActionCache, Debug bool) (*CommandServer, error) {
	tapFSData := newTapFSData(cas)
	tapFSData.registerPGID(1)

	var tapRoot fsAPI
	switch subtype := root.(type) {
	case *fs.LoopbackNode:
		tapRoot = NewLoopbackTapFS(subtype, tapFSData)
	case *gritfs.RepoNode:
		tapRoot = NewGritFSRoot(subtype, tapFSData)
	default:
		return nil, fmt.Errorf("unsupported root type %T", root)
	}

	sec := time.Second
	fsServer, err := fs.Mount(mntDir, tapRoot, &fs.Options{
		MountOptions: fuse.MountOptions{Debug: Debug},
		UID:          uint32(os.Getuid()),
		GID:          uint32(os.Getgid()),
		EntryTimeout: &sec,
		AttrTimeout:  &sec,
		// not allowed for loopback based FS.
		NegativeTimeout: nil,
	})
	if err != nil {
		return nil, err
	}

	fsServer.WaitMount()
	syscall.Access(mntDir, 07)

	l, sock, err := newSocket()
	if err != nil {
		return nil, err
	}

	ch := tapRoot.EmbeddedInode().NewPersistentInode(context.Background(), &fs.MemSymlink{
		Data: []byte(sock),
	}, fs.StableAttr{Mode: fuse.S_IFLNK})
	tapRoot.EmbeddedInode().AddChild(socketName, ch, true)

	commandServer := &CommandServer{
		tapFSData:  tapFSData,
		FSServer:   fsServer,
		mountPoint: mntDir,
		root:       tapRoot,
		cas:        cas,
		ac:         ac,
		Debug:      Debug,
		listener:   l,
	}
	srv := rpc.NewServer()
	if err := srv.Register(commandServer); err != nil {
		return nil, err
	}

	go srv.Accept(l)

	return commandServer, nil
}

func (s *CommandServer) Addr() string {
	return s.listener.Addr().String()
}

func FindSocket(startDir string) (socket string, topdir string, err error) {
	for dir := startDir; dir != "/"; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, socketName)
		fi, err := os.Stat(p)
		if fi == nil || fi.Mode()&iofs.ModeType != iofs.ModeSocket {
			continue
		}
		val, err := os.Readlink(p)
		if err != nil {
			return "", "", err
		}
		if filepath.IsAbs(val) {
			return val, dir, nil
		}
	}

	return "", "", fmt.Errorf("socket %q not found", socketName)
}

type TraceRequest struct {
	PGID            int
	Command         string
	Dir             string
	DeclaredInputs  []string
	DeclaredOutputs []string

	// StartTrace: try to populate from cache, endtrace: try to store cache.
	Cache bool

	// Only for EndTrace
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type CacheHit struct {
	Stderr []byte
	Stdout []byte
}

type FileType byte

const (
	FileRegular    = FileType(0)
	FileExecutable = FileType(1)
	FileSymlink    = FileType(2)
)

func (ft FileType) Mode() filemode.FileMode {
	switch ft {
	case FileRegular:
		return filemode.Regular
	case FileSymlink:
		return filemode.Symlink
	case FileExecutable:
		return filemode.Executable
	default:
		log.Panicf("mode %o", ft)
	}

	return 0
}

func FileTypeFromMode(mode uint32) FileType {
	if mode&^07777 == fuse.S_IFLNK {
		return FileSymlink
	}
	if mode&0111 != 0 {
		return FileExecutable
	}
	return FileRegular
}

type FileInfo struct {
	Digest Digest
	Type   FileType
}

type TraceResponse struct {
	// only populated for EndTrace
	ID string

	Files      map[string]FileInfo
	Operations map[string]Operation

	// If set, don't run command.
	CacheHit *CacheHit
}

func (s *CommandServer) StartTrace(req *TraceRequest, rep *TraceResponse) error {
	if req.Cache {
		if err := s.checkActionCache(req, rep); err == nil {
			return nil
		} else if err == acNotFound {
			// nothing
		} else {
			return err
		}
	}

	od := s.tapFSData.registerPGID(req.PGID)

	if s.Debug && false {
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.debugLog != nil {
			log.Printf("already have a debug log. Are you using -j1 ?")
		} else {
			depDir := ""
			f, err := os.Create(filepath.Join(depDir, fmt.Sprintf("%s.log", od.id)))
			if err != nil {
				return err
			}
			s.debugLog = f
			log.SetOutput(f)
		}
	}

	return nil
}

func (s *CommandServer) EndTrace(req *TraceRequest, rep *TraceResponse) error {
	od := s.tapFSData.removeRecord(req.PGID)
	if od == nil {
		return fmt.Errorf("no trace for pgid %d", req.PGID)
	}
	rep.ID = od.id
	rep.Files = map[string]FileInfo{}
	rep.Operations = map[string]Operation{}
	for n, op := range od.ops {
		if _, p := n.Parent(); p == nil {
			continue
		}
		path := n.Path(nil)
		delete(od.deletions, path)

		fi, err := s.root.hashForPath(path)
		if err != nil {
			return err
		}
		rep.Files[path] = fi
		rep.Operations[path] = op
	}
	for path := range od.deletions {
		rep.Operations[path] = OpDelete
	}

	if s.Debug && false {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.debugLog != nil {
			log.SetOutput(os.Stderr)
			s.debugLog.Close()
			s.debugLog = nil
		} else {
			log.Printf("EndTrace called, but debugLog == nil")
		}
	}
	if req.ExitCode == 0 && req.Cache {
		if err := s.storeAction(req, rep); err != nil {
			return err
		}
	}
	return nil
}

func newSocket() (net.Listener, string, error) {
	dir, err := os.MkdirTemp("", "newSocket")
	if err != nil {
		return nil, "", err
	}
	s := filepath.Join(dir, "socket")
	l, err := net.Listen("unix", s)
	return l, s, err
}

var ioRegex = regexp.MustCompile(`^ninja_inputs='([^']*)'; ninja_outputs='([^']*)'; `)

// Runs a command on the server for use in the command-line program
func ClientRun(socket string, commandline string, env []string, dir string, cache bool) (*TraceResponse, error) {
	client, err := rpc.Dial("unix", socket)
	if err != nil {
		return nil, err
	}

	pid := os.Getpid()
	if err := syscall.Setpgid(pid, 0); err != nil {
		return nil, err
	}

	req := TraceRequest{
		PGID:  pid,
		Cache: cache,
	}

	groups := ioRegex.FindStringSubmatch(commandline)
	if groups != nil {
		req.DeclaredInputs = strings.Split(strings.TrimSpace(groups[1]), " ")
		req.DeclaredOutputs = strings.Split(strings.TrimSpace(groups[2]), " ")
		sort.Strings(req.DeclaredInputs)
		sort.Strings(req.DeclaredOutputs)
		req.Command = strings.TrimSpace(commandline[len(groups[0]):])
	} else {
		req.Command = strings.TrimSpace(commandline)
	}

	var rep TraceResponse
	if err := client.Call("CommandServer.StartTrace", &req, &rep); err != nil {
		return nil, err
	}

	if rep.CacheHit != nil {
		return &rep, nil
	}

	cmd := exec.Command("/bin/sh", "-c", req.Command)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(&outBuf, os.Stdout)
	cmd.Stderr = io.MultiWriter(&errBuf, os.Stderr)
	cmd.Stdin = os.Stdin
	cmd.Dir = dir
	cmd.Env = env

	err = cmd.Run()

	req.ExitCode = cmd.ProcessState.ExitCode()
	req.Stderr = errBuf.Bytes()
	req.Stdout = outBuf.Bytes()
	err2 := client.Call("CommandServer.EndTrace", &req, &rep)
	if err != nil {
		return nil, err
	}
	if err2 != nil {
		return nil, err2
	}
	return &rep, nil
}

func (s *CommandServer) storeAction(req *TraceRequest, rep *TraceResponse) error {
	val, err := actionCacheValue(req, rep)
	if err != nil {
		return err
	}
	key := actionCacheKey(req, rep.Files)

	return s.ac.Put(key, val)
}

var acNotFound = errors.New("action cache not found")

func nodeAt(n *fs.Inode, path string) (*fs.Inode, string) {
	for n != nil && len(path) > 0 {
		idx := strings.Index(path, "/")
		var comp, nextPath string
		if idx > 0 {
			comp = path[:idx]
			nextPath = path[idx+1:]
		} else {
			comp = path
			nextPath = ""
		}

		next := n.GetChild(comp)
		if next == nil {
			return n, path
		}

		n = next
		path = nextPath
	}
	return n, path
}

func (s *CommandServer) checkActionCache(req *TraceRequest, rep *TraceResponse) error {
	inHash := map[string]FileInfo{}
	for _, in := range req.DeclaredInputs {
		h, err := s.root.hashForPath(in)
		if err != nil {
			return acNotFound
		}
		inHash[in] = h
	}

	key := actionCacheKey(req, inHash)
	val, err := s.ac.Get(key)
	if err != nil {
		return err
	}

	if val == nil {
		return acNotFound
	}

	for in, inH := range val.Inputs {
		if _, ok := inHash[in]; ok {
			continue
		}
		h, err := s.root.hashForPath(in)
		if err != nil {
			return acNotFound
		}

		if h != inH {
			// undeclared dep.
			return nil
		}
	}

	rep.CacheHit = &CacheHit{
		Stderr: val.Stderr,
		Stdout: val.Stdout,
	}

	start := time.Now()
	err = s.root.fromActionCache(val)
	dt := time.Now().Sub(start)
	log.Printf("cache hit for: %s (pgid %d), took %v to populate", req.Command, req.PGID, dt)
	return err
}

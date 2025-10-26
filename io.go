package tapfs

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// CommandServer serves RPC calls
type CommandServer struct {
	root       *TapFSRoot
	mountPoint string
	FSServer   *fuse.Server
	cas        CAS
	ac         ActionCache
	mu         sync.Mutex
	Debug      bool
	debugLog   io.WriteCloser
	listener   net.Listener
}

func (c *CommandServer) Close() error {
	return c.listener.Close()
}
func (c *CommandServer) Wait() {
	c.FSServer.Wait()
}

const socketName = ".tapfs"

func NewCommandServer(root fs.InodeEmbedder, mntDir string, cas CAS, ac ActionCache, Debug bool) (*CommandServer, error) {
	tapRoot := NewTapFS(root.(*fs.LoopbackNode))

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

	ch := tapRoot.NewPersistentInode(context.Background(), &fs.MemSymlink{
		Data: []byte(sock),
	}, fs.StableAttr{Mode: fuse.S_IFLNK})
	tapRoot.AddChild(socketName, ch, true)

	commandServer := &CommandServer{
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
)

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

type JSONOpenData struct {
	ID      string
	Command string
	Dir     string

	Read   []string
	Create []string
	Update []string
	Delete []string

	Hashes map[string]Digest
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

	od := s.root.registerPGID(req.PGID)

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
	od := s.root.removeRecord(req.PGID)
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

		fi, err := s.hashForPath(path)
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

func (s *CommandServer) writeJson(od *openData, req *TraceRequest, rep *TraceResponse) error {
	depDir := ""
	fn := filepath.Join(depDir, od.id) + ".json"

	jsonOD := JSONOpenData{
		ID:      rep.ID,
		Command: req.Command,
		Dir:     req.Dir,
		Hashes:  map[string]Digest{},
	}
	for path, f := range rep.Files {
		jsonOD.Hashes[path] = f.Digest
	}
	dests := map[Operation]*[]string{
		OpRead:   &jsonOD.Read,
		OpCreate: &jsonOD.Create,
		OpUpdate: &jsonOD.Update,
		OpDelete: &jsonOD.Delete,
	}
	for p, op := range rep.Operations {
		*dests[op] = append(*dests[op], p)
	}
	for _, v := range dests {
		sort.Strings(*v)
	}

	if data, err := json.Marshal(jsonOD); err != nil {
		return err
	} else if err := os.WriteFile(fn, data, 0644); err != nil {
		return err
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

func (s *CommandServer) hashForPath(path string) (fi FileInfo, err error) {
	err = func() error {
		full := filepath.Join(s.root.LoopbackNode.RootData.Path, path)
		if _, err := os.Lstat(full); err != nil {
			return err
		}

		n, left := nodeAt(s.root.EmbeddedInode(), path)
		if left != "" || n == nil {
			return fmt.Errorf("can't traverse %q: %q / %q", path, n.Path(nil), left)
		}
		if tf, ok := n.Operations().(*TapFSNode); ok {
			fi, err = tf.GetFileInfo(s.cas)
			if err != nil {
				return err
			}
			return nil
		}

		return fmt.Errorf("not a TapFSNode")
	}()
	return fi, err
}

func (s *CommandServer) checkActionCache(req *TraceRequest, rep *TraceResponse) error {
	inHash := map[string]FileInfo{}
	for _, in := range req.DeclaredInputs {
		h, err := s.hashForPath(in)
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
		h, err := s.hashForPath(in)
		if err != nil {
			return acNotFound
		}

		if h != inH {
			// undeclared dep.
			return nil
		}
	}
	log.Printf("cache hit for: %s (pgid %d)", req.Command, req.PGID)

	rep.CacheHit = &CacheHit{
		Stderr: val.Stderr,
		Stdout: val.Stdout,
	}

	return s.fromActionCache(val)
}

func (s *CommandServer) fromActionCache(val *ActionCacheValue) error {
	for out, fileInfo := range val.Outputs {
		r, err := s.cas.Get(fileInfo.Digest)
		if err != nil {
			return err
		}

		mode := 0644
		switch fileInfo.Type {
		case FileExecutable:
			mode = 0755
		}
		orig := filepath.Join(s.root.RootData.Path, out)
		if err := os.MkdirAll(filepath.Dir(orig), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(orig, os.O_CREATE|os.O_WRONLY, os.FileMode(mode))
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(f, r); err != nil {
			return err
		}

		if err := f.Close(); err != nil {
			return err
		}

		// Make the create kernel the FUSE node.
		if _, err := os.Lstat(filepath.Join(s.mountPoint, out)); err != nil {
			return fmt.Errorf("lstat refresh %s: %v", out, err)
		}
		r.Close()
	}

	rootInode := s.root.EmbeddedInode()
	for out, fileInfo := range val.Outputs {
		ch, left := nodeAt(rootInode, out)
		if ch == nil {
			log.Printf("path %q no child: node nil, left %s", out, left)
			continue
		}
		tf := ch.Operations().(*TapFSNode)
		tf.mu.Lock()
		tf.fileInfo = fileInfo
		tf.mu.Unlock()
	}

	return nil
}

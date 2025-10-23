package tapfs

import (
	"bytes"
	"context"
	"crypto/sha256"
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

type hashKey struct {
	Ino  uint64
	Mtim syscall.Timespec
	Size int64
}

func (k *hashKey) FromStat(st *syscall.Stat_t) {
	k.Ino = st.Ino
	k.Mtim = st.Mtim
	k.Size = st.Size
}

// CommandServer serves RPC calls
type CommandServer struct {
	root     *TapFSRoot
	FSServer *fuse.Server
	depDir   string
	cas      *CAS
	ac       *ActionCache
	mu       sync.Mutex
	Debug    bool
	debugLog io.WriteCloser
	listener net.Listener
	hashes   map[hashKey]string
}

func (c *CommandServer) Close() error {
	return c.listener.Close()
}
func (c *CommandServer) Wait() {
	c.FSServer.Wait()
}

const socketName = ".tapfs"

func NewCommandServer(root fs.InodeEmbedder, mntDir, dbDir string, Debug bool) (*CommandServer, error) {
	tapRoot := NewTapFS(root.(*fs.LoopbackNode))

	sec := time.Second
	fsServer, err := fs.Mount(mntDir, tapRoot, &fs.Options{
		MountOptions:    fuse.MountOptions{Debug: Debug},
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
		EntryTimeout:    &sec,
		AttrTimeout:     &sec,
		NegativeTimeout: &sec,
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
	depDir := filepath.Join(dbDir, "deps")
	casDir := filepath.Join(dbDir, "cas")
	os.MkdirAll(depDir, 0755)
	os.MkdirAll(casDir, 0755)

	ch := tapRoot.NewPersistentInode(context.Background(), &fs.MemSymlink{
		Data: []byte(sock),
	}, fs.StableAttr{Mode: fuse.S_IFLNK})
	tapRoot.AddChild(socketName, ch, true)
	cas := NewCAS(casDir, sha256.New)

	commandServer := &CommandServer{
		FSServer: fsServer,
		root:     tapRoot,
		cas:      cas,
		ac:       NewActionCache(),
		depDir:   depDir,
		Debug:    Debug,
		listener: l,
		hashes:   map[hashKey]string{},
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

	// Only for EndTrace
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type CacheHit struct {
	Stderr []byte
	Stdout []byte
}

type TraceResponse struct {
	// only populated for EndTrace
	ID     string
	DepDir string
	Read   []string
	Create []string
	Update []string
	Delete []string

	Hashes map[string]string

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

	Hashes map[string]string
}

func (s *CommandServer) StartTrace(req *TraceRequest, rep *TraceResponse) error {
	if err := s.checkActionCache(req, rep); err == nil {
		return nil
	} else if err == acNotFound {
		// nothing
	} else {
		return err
	}

	od := s.root.registerPGID(req.PGID)

	if s.Debug {
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.debugLog != nil {
			log.Printf("already have a debug log. Are you using -j1 ?")
		} else {
			f, err := os.Create(filepath.Join(s.depDir, fmt.Sprintf("%s.log", od.id)))
			if err != nil {
				return err
			}
			s.debugLog = f
			log.SetOutput(f)
		}
	}

	return nil
}

func (s *CommandServer) hashForPath(p string) (string, error) {
	orig := filepath.Join(s.root.RootData.Path, p)
	var st syscall.Stat_t
	if err := syscall.Stat(orig, &st); err != nil {
		return "", err
	}

	var key hashKey
	key.FromStat(&st)
	s.mu.Lock()
	h, ok := s.hashes[key]
	s.mu.Unlock()

	if !ok {
		f, err := os.Open(orig)
		if err != nil {
			return "", err
		}

		h, err = s.cas.Add(f)
		f.Close()
		if err != nil {
			return "", err
		}

		s.mu.Lock()
		s.hashes[key] = h
		s.mu.Unlock()
	}

	return h, nil
}

func (s *CommandServer) EndTrace(req *TraceRequest, rep *TraceResponse) error {
	od := s.root.removeRecord(req.PGID)
	rep.ID = od.id
	rep.DepDir = s.depDir
	rep.Hashes = map[string]string{}

	dests := map[operation]*[]string{
		opRead:   &rep.Read,
		opCreate: &rep.Create,
		opUpdate: &rep.Update,
		opDelete: &rep.Delete,
	}

	for n, op := range od.ops {
		if _, p := n.Parent(); p == nil {
			continue
		}
		path := n.Path(nil)
		log.Println(path)
		delete(od.deletions, path)
		dest := dests[op]
		*dest = append(*dest, path)

		h, err := s.hashForPath(path)
		if err != nil {
			return err
		}
		rep.Hashes[path] = h
	}
	for path := range od.deletions {
		rep.Delete = append(rep.Delete, path)
		rep.Hashes[path] = s.cas.Zero()
	}

	for _, dest := range dests {
		sort.Strings(*dest)
	}

	if s.Debug {
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

	fn := filepath.Join(s.depDir, od.id) + ".json"

	jsonOD := JSONOpenData{
		ID:      rep.ID,
		Command: req.Command,
		Dir:     req.Dir,
		Read:    rep.Read,
		Create:  rep.Create,
		Update:  rep.Update,
		Delete:  rep.Delete,
		Hashes:  rep.Hashes,
	}

	if data, err := json.Marshal(jsonOD); err != nil {
		return err
	} else if err := os.WriteFile(fn, data, 0644); err != nil {
		return err
	}

	if req.ExitCode == 0 {
		s.storeAction(req, rep)
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
func ClientRun(socket string, commandline string, env []string, dir string) (*TraceResponse, error) {
	client, err := rpc.Dial("unix", socket)
	if err != nil {
		return nil, err
	}

	pid := os.Getpid()
	if err := syscall.Setpgid(pid, 0); err != nil {
		return nil, err
	}

	req := TraceRequest{
		PGID: pid,
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
	key := actionCacheKey(req, rep.Hashes)

	return s.ac.Put(key, val)
}

var acNotFound = errors.New("action cache not found")

func (s *CommandServer) checkActionCache(req *TraceRequest, rep *TraceResponse) error {
	inHash := map[string]string{}
	for _, in := range req.DeclaredInputs {
		h, err := s.hashForPath(in)
		if err != nil {
			return err
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

		if h, err := s.hashForPath(in); err != nil {
			// don't propagate. Maybe ENOENT
			return nil
		} else if h != inH {
			log.Printf("cache miss due to undeclared dep %q", in)
			return nil
		}
	}
	log.Printf("cache hit for: %s", req.Command)

	rep.CacheHit = &CacheHit{
		Stderr: val.Stderr,
		Stdout: val.Stdout,
	}

	return s.fromActionCache(val)
}

func (s *CommandServer) fromActionCache(val *ActionCacheValue) error {
	for out, outHash := range val.Outputs {
		r, err := s.cas.Get(outHash)
		if err != nil {
			return err
		}

		f, err := os.Create(filepath.Join(s.root.RootData.Path, out))
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(f, r); err != nil {
			return err
		}

		var st syscall.Stat_t
		if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
			return err
		}

		var k hashKey
		k.FromStat(&st)

		s.mu.Lock()
		s.hashes[k] = outHash
		s.mu.Unlock()

		if err := f.Close(); err != nil {
			return err
		}

		r.Close()
	}
	return nil
}

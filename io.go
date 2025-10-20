package tapfs

import (
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

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type hashKey struct {
	Ino  uint64
	Mtim syscall.Timespec
	Size int64
}

// CommandServer serves RPC calls
type CommandServer struct {
	root     *TapFSRoot
	depDir   string
	cas      *CAS
	ac       *ActionCache
	mu       sync.Mutex
	Debug    bool
	debugLog io.WriteCloser

	hashes map[hashKey]string
}

const socketName = ".tapfs"

func StartCommandServer(root *TapFSRoot, dir string, server *fuse.Server, Debug bool) error {
	server.WaitMount()
	l, sock, err := newSocket()
	if err != nil {
		return err
	}
	depDir := filepath.Join(dir, "deps")
	casDir := filepath.Join(dir, "cas")
	os.MkdirAll(depDir, 0755)
	os.MkdirAll(casDir, 0755)

	ch := root.NewPersistentInode(context.Background(), &fs.MemSymlink{
		Data: []byte(sock),
	}, fs.StableAttr{Mode: fuse.S_IFLNK})
	root.AddChild(socketName, ch, true)
	cas := NewCAS(casDir, sha256.New)

	commandServer := &CommandServer{
		root:   root,
		cas:    cas,
		ac:     NewActionCache(),
		depDir: depDir,
		Debug:  Debug,
		hashes: map[hashKey]string{},
	}
	srv := rpc.NewServer()
	if err := srv.Register(commandServer); err != nil {
		return err
	}
	go srv.Accept(l)

	return nil
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
	ExitCode int
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
	CacheHit bool
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
	if err := s.checkActionCache(req); err == nil {
		rep.CacheHit = true
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

	key := hashKey{Ino: st.Ino, Mtim: st.Mtim, Size: st.Size}
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

	for _, v := range []struct {
		op   operation
		dest *[]string
	}{
		{opRead, &rep.Read},
		{opCreate, &rep.Create},
		{opUpdate, &rep.Update},
		{opDelete, &rep.Delete},
	} {
		for k := range od.ops[v.op] {
			*v.dest = append(*v.dest, k)
			if v.op == opDelete {
				rep.Hashes[k] = s.cas.Zero()
				continue
			}

			h, err := s.hashForPath(k)
			if err != nil {
				return err
			}
			rep.Hashes[k] = h
		}
		sort.Strings(*v.dest)
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
func ClientRun(socket string, commandline string, env []string, dir string) error {
	client, err := rpc.Dial("unix", socket)
	if err != nil {
		return err
	}

	pid := os.Getpid()
	if err := syscall.Setpgid(pid, 0); err != nil {
		return err
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
		log.Panicf("boom %q", commandline)
	}

	var rep TraceResponse
	if err := client.Call("CommandServer.StartTrace", &req, &rep); err != nil {
		return err
	}

	if rep.CacheHit {
		return nil
	}

	cmd := exec.Command("/bin/sh", "-c", req.Command)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Dir = dir
	cmd.Env = env

	err = cmd.Run()

	req.ExitCode = cmd.ProcessState.ExitCode()

	err2 := client.Call("CommandServer.EndTrace", &req, &rep)
	if err != nil {
		return err
	}
	if err2 != nil {
		return err2
	}
	return nil
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

func (s *CommandServer) checkActionCache(req *TraceRequest) error {
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

	log.Printf("cache hit for: %s", req.Command)

	for in, inH := range val.Inputs {
		if _, ok := inHash[in]; ok {
			continue
		}

		if h, err := s.hashForPath(in); err != nil {
			return fmt.Errorf("hashForPath: %v", err)
		} else if h != inH {
			log.Printf("cache miss due to undeclared dep %q", in)
			return nil
		}
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

		if _, err := io.Copy(f, r); err != nil {
			return err
		}

		if err := f.Close(); err != nil {
			return err
		}

		r.Close()
	}
	return nil
}

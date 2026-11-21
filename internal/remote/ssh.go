package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSH is a Host backed by an SSH connection (with optional ProxyJump).
type SSH struct {
	client *ssh.Client
	jumps  []*ssh.Client
	sftp   *sftp.Client
}

type endpoint struct {
	user, host, port string
	identities       []string
	proxyJump        string
}

// userConfig decodes $HOME/.ssh/config (the library's own lookup ignores $HOME).
func userConfig() *ssh_config.Config {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, err := ssh_config.Decode(f)
	if err != nil {
		return nil
	}
	return cfg
}

// resolve turns "alias", "host", "user@host" or "user@host:port" into an
// endpoint, applying ~/.ssh/config for the alias.
func resolve(target string) endpoint {
	var e endpoint
	if at := strings.LastIndex(target, "@"); at >= 0 {
		e.user, target = target[:at], target[at+1:]
	}
	if h, p, err := net.SplitHostPort(target); err == nil {
		target, e.port = h, p
	}
	cfg := userConfig()
	get := func(key string) string {
		if cfg == nil {
			return ""
		}
		v, _ := cfg.Get(target, key)
		return v
	}
	if e.host = get("HostName"); e.host == "" {
		e.host = target
	}
	if e.port == "" {
		e.port = get("Port")
	}
	if e.port == "" {
		e.port = "22"
	}
	if e.user == "" {
		e.user = get("User")
	}
	if e.user == "" {
		e.user = os.Getenv("USER")
	}
	if cfg != nil {
		ids, _ := cfg.GetAll(target, "IdentityFile")
		for _, id := range ids {
			e.identities = append(e.identities, expandHome(id))
		}
	}
	if pj := get("ProxyJump"); pj != "" && !strings.EqualFold(pj, "none") {
		e.proxyJump = pj
	}
	return e
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func knownHostsFiles() []string {
	home, _ := os.UserHomeDir()
	var files []string
	for _, f := range []string{filepath.Join(home, ".ssh", "known_hosts"), filepath.Join(home, ".ssh", "known_hosts2")} {
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	return files
}

func authMethods(identities []string) []ssh.AuthMethod {
	var signers []ssh.Signer
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			if s, err := agent.NewClient(conn).Signers(); err == nil {
				signers = append(signers, s...)
			}
		}
	}
	if len(identities) == 0 {
		home, _ := os.UserHomeDir()
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			identities = append(identities, filepath.Join(home, ".ssh", n))
		}
	}
	for _, id := range identities {
		raw, err := os.ReadFile(id)
		if err != nil {
			continue
		}
		if s, err := ssh.ParsePrivateKey(raw); err == nil { // encrypted keys must come from the agent
			signers = append(signers, s)
		}
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}
}

// clientConfig enforces known_hosts. It never falls back to InsecureIgnoreHostKey.
func clientConfig(e endpoint) (*ssh.ClientConfig, error) {
	files := knownHostsFiles()
	if len(files) == 0 {
		return nil, errors.New("~/.ssh/known_hosts não existe: conecte uma vez com ssh e confira a chave do servidor")
	}
	cb, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("lendo known_hosts: %w", err)
	}
	addr := net.JoinHostPort(e.host, e.port)
	return &ssh.ClientConfig{
		User:              e.user,
		Auth:              authMethods(e.identities),
		HostKeyCallback:   cb,
		HostKeyAlgorithms: knownAlgorithms(cb, addr),
		Timeout:           15 * time.Second,
	}, nil
}

// knownAlgorithms asks the known_hosts callback which key types it holds for
// addr, so the server is asked for a key type we can actually verify.
func knownAlgorithms(cb ssh.HostKeyCallback, addr string) []string {
	tcp, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		tcp = &net.TCPAddr{}
	}
	err = cb(addr, tcp, probeKey{})
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) {
		return nil
	}
	var algos []string
	for _, k := range ke.Want {
		if t := k.Key.Type(); t == ssh.KeyAlgoRSA {
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		} else {
			algos = append(algos, t)
		}
	}
	return algos
}

type probeKey struct{}

func (probeKey) Type() string                        { return "madeploy-probe" }
func (probeKey) Marshal() []byte                     { return []byte("madeploy-probe") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe") }

// Dial connects to target, following ProxyJump (one or more comma-separated hops).
func Dial(target string) (*SSH, error) {
	e := resolve(target)
	s := &SSH{}
	var base *ssh.Client
	if e.proxyJump != "" {
		for _, hop := range strings.Split(e.proxyJump, ",") {
			c, err := dialVia(base, resolve(strings.TrimSpace(hop)))
			if err != nil {
				s.Close()
				return nil, fmt.Errorf("jump host %s: %w", hop, err)
			}
			s.jumps = append(s.jumps, c)
			base = c
		}
	}
	c, err := dialVia(base, e)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("ssh %s: %w", target, err)
	}
	s.client = c
	if s.sftp, err = sftp.NewClient(c); err != nil {
		s.Close()
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return s, nil
}

func dialVia(via *ssh.Client, e endpoint) (*ssh.Client, error) {
	cfg, err := clientConfig(e)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(e.host, e.port)
	if via == nil {
		return ssh.Dial("tcp", addr, cfg)
	}
	conn, err := via.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

func (s *SSH) Stream(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	sess, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("abrindo sessão ssh: %w", err)
	}
	defer sess.Close()
	var stderr bytes.Buffer
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return &ExitError{Cmd: cmd, Stderr: stderr.String(), Err: err}
		}
		return nil
	}
}

func (s *SSH) ReadFile(p string) ([]byte, error) {
	f, err := s.sftp.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// WriteFile writes to a temp name and renames it, so readers never see a partial file.
func (s *SSH) WriteFile(p string, data []byte, mode os.FileMode) error {
	if err := s.sftp.MkdirAll(path.Dir(p)); err != nil {
		return err
	}
	tmp := p + ".tmp-madeploy"
	f, err := s.sftp.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := s.sftp.Chmod(tmp, mode); err != nil {
		return err
	}
	return s.sftp.PosixRename(tmp, p)
}

func (s *SSH) Upload(localPath, remotePath string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := s.sftp.MkdirAll(path.Dir(remotePath)); err != nil {
		return err
	}
	dst, err := s.sftp.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return s.sftp.Chmod(remotePath, 0o600)
}

// Tunnel forwards a local ephemeral port to addr as seen from the server.
func (s *SSH) Tunnel(addr string) (local string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			lc, err := ln.Accept()
			if err != nil {
				return
			}
			rc, err := s.client.Dial("tcp", addr)
			if err != nil {
				lc.Close()
				continue
			}
			go pipe(lc, rc)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }, nil
}

func pipe(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

func (s *SSH) Close() error {
	if s.sftp != nil {
		s.sftp.Close()
	}
	if s.client != nil {
		s.client.Close()
	}
	for i := len(s.jumps) - 1; i >= 0; i-- {
		s.jumps[i].Close()
	}
	return nil
}

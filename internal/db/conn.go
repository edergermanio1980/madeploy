package db

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/vitalsoft/madeploy/internal/remote"
)

// Conn are the credentials of one Adianti connection. Pass is never printed.
type Conn struct {
	Name string // connection name (sisvita, permission, ...)
	Type string
	Host string
	Port int
	DB   string
	User string
	Pass string
}

func (c Conn) String() string {
	return fmt.Sprintf("%s (%s@%s:%d/%s)", c.Name, c.User, c.Host, c.Port, c.DB)
}

var phpKV = regexp.MustCompile(`['"](\w+)['"]\s*=>\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)"|(\d+))`)

// ParseConnection reads an Adianti app/config/<conn>.php (`return [...]`)
// without executing PHP. Env vars MADEPLOY_<CONN>_{HOST,PORT,NAME,USER,PASS} override.
func ParseConnection(name string, src []byte) (Conn, error) {
	kv := map[string]string{}
	for _, m := range phpKV.FindAllStringSubmatch(string(src), -1) {
		v := m[2] + m[3] + m[4]
		kv[strings.ToLower(m[1])] = strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(v)
	}
	pick := func(key, fallback string) string {
		if v := os.Getenv("MADEPLOY_" + strings.ToUpper(name) + "_" + strings.ToUpper(key)); v != "" {
			return v
		}
		if v := kv[key]; v != "" {
			return v
		}
		return fallback
	}
	c := Conn{Name: name, Type: pick("type", ""), Host: pick("host", "127.0.0.1"), DB: pick("name", ""), User: pick("user", ""), Pass: pick("pass", "")}
	port, err := strconv.Atoi(pick("port", "3306"))
	if err != nil {
		return c, fmt.Errorf("%s: porta inválida", name)
	}
	c.Port = port
	if c.Type != "mysql" {
		return c, fmt.Errorf("%s: tipo %q não suportado (só mysql)", name, c.Type)
	}
	if c.DB == "" || c.User == "" {
		return c, fmt.Errorf("%s: config sem name/user", name)
	}
	if c.Host == "localhost" {
		c.Host = "127.0.0.1" // seen from the SSH server: TCP to its local MySQL
	}
	return c, nil
}

// Tunneler is implemented by remote.SSH.
type Tunneler interface {
	Tunnel(addr string) (string, func(), error)
}

// Session is an open connection to one database through the SSH tunnel.
type Session struct {
	Conn  Conn
	Addr  string // local address reachable by mysqldef and the driver
	DB    *sql.DB
	close func()
}

func (s *Session) Close() {
	if s.DB != nil {
		s.DB.Close()
	}
	if s.close != nil {
		s.close()
	}
}

// Open reads the production config of conn and connects through the tunnel.
// When the host cannot tunnel (tests), it connects directly.
func Open(ctx context.Context, h remote.Host, appPath, conn, configRel string) (*Session, error) {
	raw, err := h.ReadFile(path.Join(appPath, configRel))
	if err != nil {
		return nil, fmt.Errorf("lendo %s no servidor: %w", configRel, err)
	}
	c, err := ParseConnection(conn, raw)
	if err != nil {
		return nil, err
	}
	s := &Session{Conn: c, Addr: net.JoinHostPort(c.Host, strconv.Itoa(c.Port))}
	if t, ok := h.(Tunneler); ok {
		if s.Addr, s.close, err = t.Tunnel(s.Addr); err != nil {
			return nil, fmt.Errorf("túnel para %s: %w", c, err)
		}
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = c.User, c.Pass, "tcp", s.Addr, c.DB
	cfg.ParseTime = true
	cfg.Timeout = 15 * time.Second
	if s.DB, err = sql.Open("mysql", cfg.FormatDSN()); err != nil {
		s.Close()
		return nil, err
	}
	s.DB.SetMaxOpenConns(1) // migrations run on one connection, in order
	if err := s.DB.PingContext(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("conectando em %s: %w", c, err)
	}
	return s, nil
}

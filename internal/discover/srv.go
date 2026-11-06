package discover

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"

	"github.com/silopost/silo-server/internal/netx"
)

// srvService is one RFC 6186 / RFC 8314 service label.
type srvService struct {
	service string
	tls     string
	imap    bool
}

// srvServices in order of preference: implicit TLS first (RFC 8314).
var srvServices = []srvService{
	{"imaps", TLSImplicit, true},
	{"imap", TLSStartTLS, true},
	{"submissions", TLSImplicit, false},
	{"submission", TLSStartTLS, false},
}

// srv reads the SRV records of the domain. A target of "." means the
// service is not offered (RFC 2782) and is skipped.
func (d *Discoverer) srv(ctx context.Context, domain string) found {
	type answer struct {
		recs []*net.SRV
		err  error
	}
	answers := make([]answer, len(srvServices))
	done := make(chan struct{}, len(srvServices))
	for i, s := range srvServices {
		go func() {
			_, recs, err := d.Resolver.LookupSRV(ctx, s.service, "tcp", domain)
			answers[i] = answer{recs, err}
			done <- struct{}{}
		}()
	}
	for range srvServices {
		<-done
	}
	var f found
	var notes []string
	for i, s := range srvServices {
		a := answers[i]
		label := "_" + s.service + "._tcp"
		if a.err != nil {
			var de *net.DNSError
			if errors.As(a.err, &de) && de.IsNotFound {
				continue
			}
			if errors.Is(a.err, context.DeadlineExceeded) || ctx.Err() != nil {
				notes = append(notes, label+": timed out")
			} else {
				notes = append(notes, label+": "+a.err.Error())
			}
			continue
		}
		srv := bestSRV(a.recs, s.tls)
		if srv == nil {
			continue
		}
		if s.imap && f.imap == nil {
			f.imap = srv
			notes = append(notes, label+" "+srv.String())
		} else if !s.imap && f.smtp == nil {
			f.smtp = srv
			notes = append(notes, label+" "+srv.String())
		}
	}
	f.loginFormat = LoginEmail
	if !f.ok() {
		if len(notes) == 0 {
			notes = append(notes, "no SRV records")
		}
		f.err = errors.New(strings.Join(notes, "; "))
		return found{err: f.err}
	}
	f.detail = strings.Join(notes, "; ")
	return f
}

// bestSRV returns the preferred usable record: lowest priority, then highest
// weight.
func bestSRV(recs []*net.SRV, mode string) *Server {
	recs = append([]*net.SRV(nil), recs...)
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Priority != recs[j].Priority {
			return recs[i].Priority < recs[j].Priority
		}
		return recs[i].Weight > recs[j].Weight
	})
	for _, r := range recs {
		host := netx.NormalizeHost(r.Target)
		if host == "" || r.Port == 0 || !validDomain(host) {
			continue
		}
		return &Server{Host: host, Port: int(r.Port), TLS: mode}
	}
	return nil
}

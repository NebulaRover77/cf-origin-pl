package cloudfrontoriginpl

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Module ID: usable as `source cloudfront_origin_pl { ... }`
func init() { caddy.RegisterModule((*Source)(nil)) }

// Source implements caddyhttp.IPRangeSource.
//
// Caddyfile:
//
//	trusted_proxies {
//	  source cloudfront_origin_pl {
//	    region us-east-1
//	    refresh 12h
//	    // pick ONE of:
//	    prefix_list_id pl-abcdef01
//	    prefix_list_name com.amazonaws.global.cloudfront.origin-facing
//	    include_ipv6 true
//	    aws_profile myprofile
//	    role_arn arn:aws:iam::123456789012:role/MyRole   # optional
//	  }
//	}
type Source struct {
	Region         string         `json:"region,omitempty"`
	PrefixListID   string         `json:"prefix_list_id,omitempty"`
	PrefixListName string         `json:"prefix_list_name,omitempty"`
	IncludeIPv6    bool           `json:"include_ipv6,omitempty"`
	Refresh        caddy.Duration `json:"refresh,omitempty"`

	// Optional auth tweaks
	AWSProfile string `json:"aws_profile,omitempty"`
	RoleARN    string `json:"role_arn,omitempty"`

	// If true, startup/refresh will fail when the final set is empty.
	RequireNonEmpty bool `json:"require_nonempty,omitempty"`
	// internal
	mu      sync.RWMutex
	current []netip.Prefix
	stopCh  chan struct{}
}

// Caddy module registration
func (*Source) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.ip_sources.cloudfront_origin_pl",
		New: func() caddy.Module { return new(Source) },
	}
}

const (
	// Defaults
	defaultRegion       = "us-east-1"
	defaultRefresh      = 12 * time.Hour
	defaultPLNameV4     = "com.amazonaws.global.cloudfront.origin-facing"
	defaultPLNameV6     = "com.amazonaws.global.cloudfront.origin-facing-ipv6"
	maxResultsPerPage   = 100
)

// Provision initializes config, loads initial ranges, starts refresher.
func (s *Source) Provision(ctx caddy.Context) error {
	if s.Region == "" {
		if env := os.Getenv("AWS_REGION"); env != "" {
			s.Region = env
		} else if env := os.Getenv("AWS_DEFAULT_REGION"); env != "" {
			s.Region = env
		} else {
			s.Region = defaultRegion
		}
	}
	if time.Duration(s.Refresh) == 0 {
		s.Refresh = caddy.Duration(defaultRefresh)
	}
	// Default names if not provided
	if s.PrefixListID == "" && s.PrefixListName == "" {
		s.PrefixListName = defaultPLNameV4
	}

	// initial fetch (fail hard if empty)
	if err := s.refreshOnce(context.Background()); err != nil {
		return err
	}
	if len(s.snapshot()) == 0 {
		if s.RequireNonEmpty {
			return fmt.Errorf("cloudfront_origin_pl: no prefixes found on initial fetch")
		}
		caddy.Log().Warn("cloudfront_origin_pl: initial fetch returned zero prefixes; running with empty set")
	}

	// background refresher
	s.stopCh = make(chan struct{})
	go func() {
		t := time.NewTicker(time.Duration(s.Refresh))
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = s.refreshOnce(context.Background()) // keep previous list on failure
			case <-s.stopCh:
				return
			}
		}
	}()
	return nil
}

// Cleanup stops the background refresher.
func (s *Source) Cleanup() error {
	if s.stopCh != nil {
		close(s.stopCh)
	}
	return nil
}

// GetIPRanges is called by Caddy in hot path; return a copy.
func (s *Source) GetIPRanges(_ *http.Request) []netip.Prefix {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]netip.Prefix, len(s.current))
	copy(out, s.current)
	return out
}

// snapshot returns a copy of the current prefix list under a read lock.
func (s *Source) snapshot() []netip.Prefix {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]netip.Prefix, len(s.current))
	copy(out, s.current)
	return out
}

// UnmarshalCaddyfile enables Caddyfile usage.
func (s *Source) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "region":
				if !d.NextArg() { return d.ArgErr() }
				s.Region = d.Val()
			case "refresh":
				if !d.NextArg() { return d.ArgErr() }
				dur, err := time.ParseDuration(d.Val())
				if err != nil { return d.Errf("invalid refresh: %v", err) }
				s.Refresh = caddy.Duration(dur)
			case "prefix_list_id":
				if !d.NextArg() { return d.ArgErr() }
				s.PrefixListID = d.Val()
			case "prefix_list_name":
				if !d.NextArg() { return d.ArgErr() }
				s.PrefixListName = d.Val()
			case "include_ipv6":
				// boolean (no arg => true, or explicit true/false)
				if d.NextArg() {
					val := strings.ToLower(d.Val())
					s.IncludeIPv6 = val == "true" || val == "1" || val == "yes"
				} else {
					s.IncludeIPv6 = true
				}
			case "aws_profile":
				if !d.NextArg() { return d.ArgErr() }
				s.AWSProfile = d.Val()
			case "role_arn":
				if !d.NextArg() { return d.ArgErr() }
				s.RoleARN = d.Val()
			case "require_nonempty":
				// boolean (no arg => true, or explicit true/false)
				if d.NextArg() {
					val := strings.ToLower(d.Val())
					s.RequireNonEmpty = val == "true" || val == "1" || val == "yes"
				} else {
					s.RequireNonEmpty = true
				}
			default:
				return d.Errf("unknown option %q", d.Val())
			}
		}
	}
	return nil
}

func (s *Source) refreshOnce(ctx context.Context) error {
	cfg, err := s.loadAWSConfig(ctx)
	if err != nil {
		return fmt.Errorf("cloudfront_origin_pl: aws config: %w", err)
	}
	ec2c := ec2.NewFromConfig(cfg)

	// Resolve the prefix list IDs (v4 and maybe v6)
	ids := []string{}
	idV4, err := s.resolvePrefixListID(ctx, ec2c, s.PrefixListID, s.PrefixListName, defaultPLNameV4)
	if err != nil {
		// IPv4 missing is unusual; warn and continue (may still succeed later)
		caddy.Log().Warn("cloudfront_origin_pl: could not resolve IPv4 prefix list; continuing",
			zap.Error(err), zap.String("region", s.Region))
	}
	if idV4 != "" { ids = append(ids, idV4) }

	if s.IncludeIPv6 {
		idV6, err := s.resolvePrefixListID(ctx, ec2c, "", defaultPLNameV6, defaultPLNameV6)
		if err != nil {
			// Common case: AWS account/region doesn't expose the -ipv6 list yet
			caddy.Log().Warn("cloudfront_origin_pl: skipping IPv6 list", zap.Error(err), zap.String("region", s.Region))
		}
		if idV6 != "" { ids = append(ids, idV6) }
	}

	// Fetch CIDRs from all requested lists
	seen := map[string]struct{}{}
	var all []netip.Prefix
	for _, id := range ids {
		pageToken := aws.String("")
		for {
			out, err := ec2c.GetManagedPrefixListEntries(ctx, &ec2.GetManagedPrefixListEntriesInput{
				PrefixListId: aws.String(id),
				MaxResults:   aws.Int32(maxResultsPerPage),
				NextToken:    pageTokenOrNil(pageToken),
			})
			if err != nil {
				return fmt.Errorf("cloudfront_origin_pl: get entries %s: %w", id, err)
			}
			for _, e := range out.Entries {
				if e.Cidr == nil { continue }
				c := *e.Cidr
				if _, ok := seen[c]; ok { continue }
				pfx, perr := netip.ParsePrefix(c)
				if perr != nil { continue } // skip malformed
				all = append(all, pfx)
				seen[c] = struct{}{}
			}
			if out.NextToken == nil || *out.NextToken == "" {
				break
			}
			pageToken = out.NextToken
		}
	}

	if len(all) == 0 {
		prev := s.snapshot()
		if len(prev) == 0 && s.RequireNonEmpty {
			return fmt.Errorf("cloudfront_origin_pl: resolved zero prefixes (empty on first load and require_nonempty=true)")
		}
		if len(prev) == 0 {
			caddy.Log().Warn("cloudfront_origin_pl: resolved zero prefixes; leaving empty set (will retry)",
				zap.String("region", s.Region))
			// keep empty
			s.mu.Lock()
			s.current = nil
			s.mu.Unlock()
			return nil
		}
		caddy.Log().Warn("cloudfront_origin_pl: refresh yielded zero prefixes; keeping previous set",
			zap.Int("previous_count", len(prev)), zap.String("region", s.Region))
		return nil
	}

	// install atomically
	s.mu.Lock()
	s.current = all
	s.mu.Unlock()
	return nil
}

func (s *Source) loadAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(s.Region),
	}
	if s.AWSProfile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(s.AWSProfile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil { return cfg, err }

	if s.RoleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		creds := stscreds.NewAssumeRoleProvider(stsClient, s.RoleARN)
		cfg.Credentials = aws.NewCredentialsCache(creds)
	}
	return cfg, nil
}

func (s *Source) resolvePrefixListID(
	ctx context.Context,
	ec2c *ec2.Client,
	explicitID string,
	name string,
	defaultName string,
) (string, error) {
	if explicitID != "" {
		return explicitID, nil
	}
	n := name
	if n == "" {
		n = defaultName
	}
	out, err := ec2c.DescribeManagedPrefixLists(ctx, &ec2.DescribeManagedPrefixListsInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("prefix-list-name"),
			Values: []string{n},
		}},
		MaxResults: aws.Int32(100),
	})
	if err != nil {
		return "", fmt.Errorf("describe prefix lists: %w", err)
	}
	for _, pl := range out.PrefixLists {
		if pl.PrefixListName != nil && *pl.PrefixListName == n && pl.PrefixListId != nil {
			return *pl.PrefixListId, nil
		}
	}
	return "", fmt.Errorf("managed prefix list %q not found in region %s", n, s.Region)
}

func pageTokenOrNil(t *string) *string {
	if t == nil || *t == "" {
		return nil
	}
	return t
}

// Interface guards
var _ caddy.Provisioner = (*Source)(nil)
var _ caddy.CleanerUpper = (*Source)(nil)
var _ caddyfile.Unmarshaler = (*Source)(nil)
var _ caddyhttp.IPRangeSource = (*Source)(nil)
var _ caddy.Module = (*Source)(nil)

package composer

import (
	"os"
	"path/filepath"
	"strings"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/xerrors"
	"gopkg.in/yaml.v2"

	"github.com/aquasecurity/trivy-db/pkg/db"
	"github.com/aquasecurity/trivy-db/pkg/types"
	"github.com/aquasecurity/trivy-db/pkg/vulnsrc/vulnerability"
)

const composerDir = "php-security-advisories"

var source = types.DataSource{
	Name: "PHP Security Advisories Database",
	URL:  "https://github.com/FriendsOfPHP/security-advisories",
}

type RawAdvisory struct {
	Cve       string
	Title     string
	Link      string
	Reference string
	Branches  map[string]Branch
}

type Branch struct {
	Versions []string `json:",omitempty"`
}

type Advisory struct {
	VulnerabilityID string            `json:",omitempty"`
	Branches        map[string]Branch `json:",omitempty"`
}

type VulnSrc struct {
	dbc db.Operation
}

func NewVulnSrc() VulnSrc {
	return VulnSrc{
		dbc: db.Config{},
	}
}

func (vs VulnSrc) Name() string {
	return vulnerability.PhpSecurityAdvisories
}

func (vs VulnSrc) Update(dir string) (err error) {
	repoPath := filepath.Join(dir, composerDir)
	if err := vs.update(repoPath); err != nil {
		return xerrors.Errorf("failed to update compose vulnerabilities: %w", err)
	}
	return nil
}

func (vs VulnSrc) update(repoPath string) error {
	err := vs.dbc.BatchUpdate(func(tx *bolt.Tx) error {
		if err := vs.dbc.PutDataSource(tx, vulnerability.PhpSecurityAdvisories, source); err != nil {
			return xerrors.Errorf("failed to put data source: %w", err)
		}
		if err := vs.walk(tx, repoPath); err != nil {
			return xerrors.Errorf("failed to walk compose advisories: %w", err)
		}
		return nil
	})
	if err != nil {
		return xerrors.Errorf("batch update failed: %w", err)
	}
	return nil
}
func (vs VulnSrc) walk(tx *bolt.Tx, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasPrefix(info.Name(), "CVE-") {
			return nil
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return xerrors.Errorf("failed to read a file: %w", err)
		}

		advisory := RawAdvisory{}
		err = yaml.Unmarshal(buf, &advisory)
		if err != nil {
			return xerrors.Errorf("failed to unmarshal YAML: %w", err)
		}

		// for detecting vulnerabilities
		vulnerabilityID := advisory.Cve
		if vulnerabilityID == "" {
			// e.g. CVE-2019-12139.yaml => CVE-2019-12139
			vulnerabilityID = strings.TrimSuffix(info.Name(), ".yaml")
		}

		var vulnerableVersions []string
		for _, branch := range advisory.Branches {
			vulnerableVersions = append(vulnerableVersions, strings.Join(branch.Versions, ", "))
		}

		a := types.Advisory{
			VulnerableVersions: vulnerableVersions,
		}
		err = vs.dbc.PutAdvisoryDetail(tx, vulnerabilityID, vulnerability.PhpSecurityAdvisories, advisory.Reference, a)
		if err != nil {
			return xerrors.Errorf("failed to save php advisory: %w", err)
		}

		// for displaying vulnerability detail
		vuln := types.VulnerabilityDetail{
			ID:         vulnerabilityID,
			References: []string{advisory.Link},
			Title:      advisory.Title,
		}
		if err = vs.dbc.PutVulnerabilityDetail(tx, vulnerabilityID, vulnerability.PhpSecurityAdvisories, vuln); err != nil {
			return xerrors.Errorf("failed to save php vulnerability detail: %w", err)
		}

		// for optimization
		if err = vs.dbc.PutVulnerabilityID(tx, vulnerabilityID); err != nil {
			return xerrors.Errorf("failed to save the vulnerability ID: %w", err)
		}
		return nil
	})
}

func (vs VulnSrc) Get(pkgName string) ([]types.Advisory, error) {
	advisories, err := vs.dbc.GetAdvisories(vulnerability.PhpSecurityAdvisories, pkgName)
	if err != nil {
		return nil, xerrors.Errorf("failed to get php vulnerabilities: %w", err)
	}

	return advisories, nil
}

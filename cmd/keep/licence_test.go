package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is two directories up from cmd/keep, where `go test` runs. The
// LICENSE file is the authority on what this project ships under; nothing
// else in the repository gets to disagree with it.
const repoRoot = "../.."

// licenceIdentifier is the SPDX identifier the LICENSE file's first line
// names, mapped from the human title it actually opens with. Only licences
// this project uses need to be here. An unrecognised title is a failure,
// not a silent pass, because a test that cannot read the authority cannot
// check anything against it.
var licenceIdentifier = map[string]string{
	"MIT License":                "MIT",
	"Apache License":             "Apache-2.0",
	"BSD 3-Clause License":       "BSD-3-Clause",
	"GNU General Public License": "GPL-3.0",
}

// TestEveryDeclaredLicenceAgreesWithTheLicenceFile checks that every manifest
// with a licence field names the licence that LICENSE grants.
//
// Nothing in the build cares whether the plugin manifests' licence fields
// agree with LICENSE, so a mismatch would otherwise survive: a manifest's
// licence field is read by a marketplace, at install time, by someone
// outside the project, who sees the mistake but cannot fix it.
//
// The check is written against whatever LICENSE says rather than against
// the string "MIT", so relicensing the project means editing one file and
// this test follows.
func TestEveryDeclaredLicenceAgreesWithTheLicenceFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil {
		t.Fatalf("no LICENSE file at the repository root: %v", err)
	}
	title := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])

	var want string
	for prefix, id := range licenceIdentifier {
		if strings.HasPrefix(title, prefix) {
			want = id
			break
		}
	}
	if want == "" {
		t.Fatalf("LICENSE opens with %q, which this test does not recognise.\n"+
			"Add it to licenceIdentifier rather than deleting the check.", title)
	}

	var checked int
	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .git is huge and .agents holds vendored third-party skills
			// whose licences are their authors' business, not ours.
			switch d.Name() {
			case ".git", ".agents", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var doc map[string]any
		if json.Unmarshal(body, &doc) != nil {
			return nil // not an object, or not JSON we can read; not our business
		}
		got, ok := doc["license"].(string)
		if !ok {
			return nil
		}
		checked++
		if got != want {
			rel, _ := filepath.Rel(repoRoot, path)
			t.Errorf("%s declares %q but LICENSE says %q (%s).\n"+
				"A marketplace shows the manifest's value to whoever installs this.",
				rel, got, want, title)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	// A walk that finds nothing passes vacuously. Two manifests declare a
	// licence today; if that reaches zero the walk has stopped working
	// rather than the manifests having stopped declaring one.
	if checked == 0 {
		t.Error("no JSON file with a \"license\" field was found at all -- the walk is not reaching the plugin manifests, so this test is asserting nothing")
	}
}

package endpoints_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/endpoints"
)

// The project the service runs in, by number, which Cloud Run's
// deterministic addresses name.
const projectNumber = "168731703350"

func TestOrganization(t *testing.T) {
	c, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	if c.Organization.ID != endpoints.Organization {
		t.Fatalf("config/organization.yaml names %d, endpoints.Organization %d", c.Organization.ID, endpoints.Organization)
	}
}

// The addresses are the ones infra/ deploys to: Cloud Run's deterministic
// URLs for the services it names, in its region, in the project.
func TestAddressesAreInfrasOwn(t *testing.T) {
	main, err := os.ReadFile("../infra/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	vars, err := os.ReadFile("../infra/variables.tf")
	if err != nil {
		t.Fatal(err)
	}
	region := regexp.MustCompile(`(?s)variable "region" \{.*?default\s*=\s*"([a-z0-9-]+)"`).FindSubmatch(vars)
	if region == nil {
		t.Fatal("infra/variables.tf has no default region")
	}
	for name, want := range map[string]string{"service_url": endpoints.Service, "proxy_url": endpoints.Proxy} {
		m := regexp.MustCompile(name + `\s*=\s*"([^"]+)"`).FindSubmatch(main)
		if m == nil {
			t.Fatalf("infra/main.tf has no %s", name)
		}
		got := strings.NewReplacer("${data.google_project.this.number}", projectNumber, "${var.region}", string(region[1])).Replace(string(m[1]))
		if got != want {
			t.Errorf("infra/ deploys %s at %s, and endpoints names %s", name, got, want)
		}
	}
}

// Copyright 2022 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"bytes"
	"strings"
	"text/template"

	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/service"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/trace"
)

// databaseAgentConfigurationTemplate database configuration template.
var databaseAgentConfigurationTemplate = template.Must(template.New("").Parse(`#
# Teleport database agent configuration file.
# Configuration reference: https://goteleport.com/docs/database-access/reference/configuration/
#
version: {{ .Version }}
teleport:
  nodename: {{ .NodeName }}
  data_dir: {{ .DataDir }}
  auth_token: {{ .AuthToken }}
  auth_servers:
  {{- range .AuthServerAddr }}
  - {{ . }}
  {{- end }}
  {{- if .CAPins }}
  ca_pins:
  {{- range .CAPins }}
  - {{ . }}
  {{- end }}
  {{- end }}
db_service:
  enabled: "yes"
  # Matchers for database resources created with "tctl create" command.
  # For more information: https://goteleport.com/docs/database-access/guides/dynamic-registration/
  resources:
  - labels:
      "*": "*"
  {{- if .RDSDiscoveryRegions }}
  # Matchers for registering AWS-hosted databases.
  aws:
  # Database types, only "rds" is supported currently.
  # For more information about RDS/Aurora auto-discovery: https://goteleport.com/docs/database-access/guides/rds/
  - types: ["rds"]
    # AWS regions to register databases from.
    regions:
    {{- range .RDSDiscoveryRegions }}
    - {{ . }}
    {{- end }}
    # AWS resource tags to match when registering databases.
    tags:
      "*": "*"
  {{- end }}
  # Lists statically registered databases proxied by this agent.
  {{- if .StaticDatabasePresent }}
  databases:
  - name: {{ .StaticDatabaseName }}
    protocol: {{ .StaticDatabaseProtocol }}
    uri: {{ .StaticDatabaseURI }}
    {{- if .StaticDatabaseStaticLabels }}
    static_labels:
    {{- range $name, $value := .StaticDatabaseStaticLabels }}
      "{{ $name }}": "{{ $value }}"
    {{- end }}
    {{- end }}
  {{- else }}
  # databases:
  # # RDS database static configuration.
  # # RDS/Aurora databases Auto-discovery reference: https://goteleport.com/docs/database-access/guides/rds/
  # - name: rds
  #   description: AWS RDS/Aurora instance configuration example.
  #   # Supported protocols for RDS/Aurora: "postgres" or "mysql"
  #   protocol: postgres
  #   # Database connection endpoint. Must be reachable from Database Service.
  #   uri: rds-instance-1.abcdefghijklmnop.us-west-1.rds.amazonaws.com:5432
  #   # AWS specific configuration.
  #   aws:
  #     # Region the database is deployed in.
  #     region: us-west-1
  #     # RDS/Aurora specific configuration.
  #     rds:
  #       # RDS Instance ID. Only present on RDS databases.
  #       instance_id: rds-instance-1
  # # Aurora database static configuration.
  # # RDS/Aurora databases Auto-discovery reference: https://goteleport.com/docs/database-access/guides/rds/
  # - name: aurora
  #   description: AWS Aurora cluster configuration example.
  #   # Supported protocols for RDS/Aurora: "postgres" or "mysql"
  #   protocol: postgres
  #   # Database connection endpoint. Must be reachable from Database Service.
  #   uri: aurora-cluster-1.abcdefghijklmnop.us-west-1.rds.amazonaws.com:5432
  #   # AWS specific configuration.
  #   aws:
  #     # Region the database is deployed in.
  #     region: us-west-1
  #     # RDS/Aurora specific configuration.
  #     rds:
  #       # Aurora Cluster ID. Only present on Aurora databases.
  #       cluster_id: aurora-cluster-1
  # # Redshift database static configuration.
  # # For more information: https://goteleport.com/docs/database-access/guides/postgres-redshift/
  # - name: redshift
  #   description: AWS Redshift cluster configuration example.
  #   # Supported protocols for Redshift: "postgres" or "mysql"
  #   protocol: postgres
  #   # Database connection endpoint. Must be reachable from Database service.
  #   uri: redshift-cluster-example-1.abcdefghijklmnop.us-west-1.redshift.amazonaws.com:5439
  #   # AWS specific configuration.
  #   aws:
  #     # Region the database is deployed in.
  #     region: us-west-1
  #     # Redshift specific configuration.
  #     redshift:
  #       # Redshift Cluster ID.
  #       cluster_id: redshift-cluster-example-1
  # # Self-hosted static configuration.
  # - name: self-hosted
  #   description: Self-hosted database configuration.
  #   # Supported protocols for self-hosted: {{ .SupportedDatabaseProtocols }}.
  #   protocol: postgres
  #   # Database connection endpoint. Must be reachable from Database service.
  #   uri: database.example.com:5432
  {{- end }}
auth_service:
  enabled: "no"
ssh_service:
  enabled: "no"
proxy_service:
  enabled: "no"`))

// DatabaseSampleFlags specifies configuration parameters for a database agent.
type DatabaseSampleFlags struct {
	// Version is the Teleport Configuration version.
	Version string
	// StaticDatabasePresent indicates if a static database is present on the
	// configuration.
	StaticDatabasePresent bool
	// StaticDatabaseName static database name provided by the user.
	StaticDatabaseName string
	// StaticDatabaseProtocol static databse protocol provided by the user.
	StaticDatabaseProtocol string
	// StaticDatabaseURI static database URI provided by the user.
	StaticDatabaseURI string
	// StaticDatabaseStaticLabels list of database static labels provided by
	// the user.
	StaticDatabaseStaticLabels map[string]string
	// StaticDatabaseDynamicLabels list of database dynamic labels provided by
	// the user.
	StaticDatabaseDynamicLabels services.CommandLabels
	// StaticDatabaseRawLabels "raw" list of database labels provided by the
	// user.
	StaticDatabaseRawLabels string
	// NodeName `nodename` configuration.
	NodeName string
	// DataDir `data_dir` configuration.
	DataDir string
	// AuthServerAddr is a list of addresses of the auth servers placed on
	// the configuration.
	AuthServerAddr []string
	// AuthToken auth server token.
	AuthToken string
	// CAPins are the SKPI hashes of the CAs used to verify the Auth Server.
	CAPins []string
	// RDSDiscoveryRegions list of regions the RDS auto-discovery is
	// configured.
	RDSDiscoveryRegions []string
	// SupportedDatabaseProtocols list of database protocols supported.
	SupportedDatabaseProtocols string
}

// CheckAndSetDefaults checks and sets default values for the flags.
func (f *DatabaseSampleFlags) CheckAndSetDefaults() error {
	conf := service.MakeDefaultConfig()
	f.Version = conf.Version
	f.SupportedDatabaseProtocols = strings.Join(defaults.DatabaseProtocols, ", ")

	if f.NodeName == "" {
		f.NodeName = conf.Hostname
	}
	if f.DataDir == "" {
		f.DataDir = conf.DataDir
	}

	if f.StaticDatabaseName != "" || f.StaticDatabaseProtocol != "" || f.StaticDatabaseURI != "" {
		f.StaticDatabasePresent = true

		if f.StaticDatabaseName == "" {
			return trace.BadParameter("must provide the database name")
		}
		if f.StaticDatabaseProtocol == "" {
			return trace.BadParameter("must provide the database protocol")
		}
		if f.StaticDatabaseURI == "" {
			return trace.BadParameter("must provide the database URI")
		}

		if f.StaticDatabaseRawLabels != "" {
			var err error
			f.StaticDatabaseStaticLabels, f.StaticDatabaseDynamicLabels, err = parseLabels(f.StaticDatabaseRawLabels)
			if err != nil {
				return trace.Wrap(err)
			}
		}
	}

	return nil
}

// MakeDatabaseAgentConfigString generates a simple database agent
// configuration based on the flags provided. Returns the configuration as a
// string.
func MakeDatabaseAgentConfigString(flags DatabaseSampleFlags) (string, error) {
	err := flags.CheckAndSetDefaults()
	if err != nil {
		return "", trace.Wrap(err)
	}

	buf := new(bytes.Buffer)
	err = databaseAgentConfigurationTemplate.Execute(buf, flags)
	if err != nil {
		return "", trace.Wrap(err)
	}

	return buf.String(), nil
}

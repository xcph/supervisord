module github.com/ochinchina/supervisord

go 1.25.0

require (
	github.com/gorilla/mux v1.8.1
	github.com/gorilla/rpc v1.2.1
	github.com/jessevdk/go-flags v1.6.1
	github.com/kardianos/service v1.2.2
	github.com/ochinchina/go-daemon v0.1.5
	github.com/ochinchina/go-ini v1.0.1
	github.com/ochinchina/go-reaper v0.0.0-20181016012355-6b11389e79fc
	github.com/ochinchina/gorilla-xmlrpc v0.0.0-20171012055324-ecf2fe693a2c
	github.com/ochinchina/supervisord/config v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/events v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/faults v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/logger v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/process v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/signals v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/types v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/util v0.0.0-20250610055946-d5a5470d11af
	github.com/ochinchina/supervisord/xmlrpcclient v0.0.0-20250610055946-d5a5470d11af
	github.com/prometheus/client_golang v1.23.2
	github.com/sirupsen/logrus v1.9.3
	github.com/xcph/cloudphone-nodeagent-api v0.0.0
	golang.org/x/sys v0.42.0
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11 // indirect; CVE-2024-24786
)

require (
	github.com/creack/pty v1.1.24
	github.com/jhump/grpctunnel v0.3.1-0.20250910230516-2ff712c4f7ff
	golang.org/x/term v0.41.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/fullstorydev/grpchan v1.1.1 // indirect
	github.com/hashicorp/go-envparse v0.1.0 // indirect
	github.com/kardianos/osext v0.0.0-20190222173326-2bc1f35cddc0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mitchellh/go-ps v1.0.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ochinchina/filechangemonitor v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.17.0 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/rogpeppe/go-charset v0.0.0-20190617161244-0dc95cdf6f31 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
)

replace github.com/xcph/cloudphone-nodeagent-api => ../cloudphone-nodeagent-api

replace (
	github.com/ochinchina/supervisord/config => ./config
	github.com/ochinchina/supervisord/events => ./events
	github.com/ochinchina/supervisord/faults => ./faults
	github.com/ochinchina/supervisord/logger => ./logger
	github.com/ochinchina/supervisord/process => ./process
	github.com/ochinchina/supervisord/signals => ./signals
	github.com/ochinchina/supervisord/types => ./types
	github.com/ochinchina/supervisord/util => ./util
	github.com/ochinchina/supervisord/xmlrpcclient => ./xmlrpcclient
)

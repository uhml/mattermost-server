// Copyright (c) 2016-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package app

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"

	"github.com/mattermost/mattermost-server/einterfaces"
	"github.com/mattermost/mattermost-server/jobs"
	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/services/httpservice"
	"github.com/mattermost/mattermost-server/utils"
	goi18n "github.com/nicksnyder/go-i18n/i18n"
)

type App struct {
	Srv *Server

	Log *mlog.Logger

	T         goi18n.TranslateFunc
	Session   model.Session
	RequestId string
	IpAddress string
	Path      string

	AccountMigration einterfaces.AccountMigrationInterface
	Cluster          einterfaces.ClusterInterface
	Compliance       einterfaces.ComplianceInterface
	DataRetention    einterfaces.DataRetentionInterface
	Elasticsearch    einterfaces.ElasticsearchInterface
	Ldap             einterfaces.LdapInterface
	MessageExport    einterfaces.MessageExportInterface
	Metrics          einterfaces.MetricsInterface
	Mfa              einterfaces.MfaInterface
	Saml             einterfaces.SamlInterface

	HTTPService httpservice.HTTPService
}

func New(options ...AppOption) *App {
	app := &App{}

	for _, option := range options {
		option(app)
	}

	return app
}

// DO NOT CALL THIS.
// This is to avoid having to change all the code in cmd/mattermost/commands/* for now
// shutdown should be called directly on the server
func (a *App) Shutdown() {
	a.Srv.Shutdown()
	a.Srv = nil
}

func (a *App) configOrLicenseListener() {
	a.regenerateClientConfig()
}

func (s *Server) initJobs() {
	s.Jobs = jobs.NewJobServer(s, s.Store)
	if jobsDataRetentionJobInterface != nil {
		s.Jobs.DataRetentionJob = jobsDataRetentionJobInterface(s.FakeApp())
	}
	if jobsMessageExportJobInterface != nil {
		s.Jobs.MessageExportJob = jobsMessageExportJobInterface(s.FakeApp())
	}
	if jobsElasticsearchAggregatorInterface != nil {
		s.Jobs.ElasticsearchAggregator = jobsElasticsearchAggregatorInterface(s.FakeApp())
	}
	if jobsElasticsearchIndexerInterface != nil {
		s.Jobs.ElasticsearchIndexer = jobsElasticsearchIndexerInterface(s.FakeApp())
	}
	if jobsLdapSyncInterface != nil {
		s.Jobs.LdapSync = jobsLdapSyncInterface(s.FakeApp())
	}
	if jobsMigrationsInterface != nil {
		s.Jobs.Migrations = jobsMigrationsInterface(s.FakeApp())
	}
	s.Jobs.Workers = s.Jobs.InitWorkers()
	s.Jobs.Schedulers = s.Jobs.InitSchedulers()
}

func (a *App) DiagnosticId() string {
	return a.Srv.diagnosticId
}

func (a *App) SetDiagnosticId(id string) {
	a.Srv.diagnosticId = id
}

func (a *App) EnsureDiagnosticId() {
	if a.Srv.diagnosticId != "" {
		return
	}
	if result := <-a.Srv.Store.System().Get(); result.Err == nil {
		props := result.Data.(model.StringMap)

		id := props[model.SYSTEM_DIAGNOSTIC_ID]
		if len(id) == 0 {
			id = model.NewId()
			systemId := &model.System{Name: model.SYSTEM_DIAGNOSTIC_ID, Value: id}
			<-a.Srv.Store.System().Save(systemId)
		}

		a.Srv.diagnosticId = id
	}
}

func (a *App) HTMLTemplates() *template.Template {
	if a.Srv.htmlTemplateWatcher != nil {
		return a.Srv.htmlTemplateWatcher.Templates()
	}

	return nil
}

func (a *App) Handle404(w http.ResponseWriter, r *http.Request) {
	err := model.NewAppError("Handle404", "api.context.404.app_error", nil, "", http.StatusNotFound)

	mlog.Debug(fmt.Sprintf("%v: code=404 ip=%v", r.URL.Path, utils.GetIpAddress(r)))

	utils.RenderWebAppError(a.Config(), w, r, err, a.AsymmetricSigningKey())
}

func (a *App) StartElasticsearch() {
	a.Srv.Go(func() {
		if err := a.Elasticsearch.Start(); err != nil {
			mlog.Error(err.Error())
		}
	})

	a.AddConfigListener(func(oldConfig *model.Config, newConfig *model.Config) {
		if !*oldConfig.ElasticsearchSettings.EnableIndexing && *newConfig.ElasticsearchSettings.EnableIndexing {
			a.Srv.Go(func() {
				if err := a.Elasticsearch.Start(); err != nil {
					mlog.Error(err.Error())
				}
			})
		} else if *oldConfig.ElasticsearchSettings.EnableIndexing && !*newConfig.ElasticsearchSettings.EnableIndexing {
			a.Srv.Go(func() {
				if err := a.Elasticsearch.Stop(); err != nil {
					mlog.Error(err.Error())
				}
			})
		} else if *oldConfig.ElasticsearchSettings.Password != *newConfig.ElasticsearchSettings.Password || *oldConfig.ElasticsearchSettings.Username != *newConfig.ElasticsearchSettings.Username || *oldConfig.ElasticsearchSettings.ConnectionUrl != *newConfig.ElasticsearchSettings.ConnectionUrl || *oldConfig.ElasticsearchSettings.Sniff != *newConfig.ElasticsearchSettings.Sniff {
			a.Srv.Go(func() {
				if *oldConfig.ElasticsearchSettings.EnableIndexing {
					if err := a.Elasticsearch.Stop(); err != nil {
						mlog.Error(err.Error())
					}
					if err := a.Elasticsearch.Start(); err != nil {
						mlog.Error(err.Error())
					}
				}
			})
		}
	})

	a.AddLicenseListener(func() {
		if a.License() != nil {
			a.Srv.Go(func() {
				if err := a.Elasticsearch.Start(); err != nil {
					mlog.Error(err.Error())
				}
			})
		} else {
			a.Srv.Go(func() {
				if err := a.Elasticsearch.Stop(); err != nil {
					mlog.Error(err.Error())
				}
			})
		}
	})
}

func (a *App) getSystemInstallDate() (int64, *model.AppError) {
	result := <-a.Srv.Store.System().GetByName(model.SYSTEM_INSTALLATION_DATE_KEY)
	if result.Err != nil {
		return 0, result.Err
	}
	systemData := result.Data.(*model.System)
	value, err := strconv.ParseInt(systemData.Value, 10, 64)
	if err != nil {
		return 0, model.NewAppError("getSystemInstallDate", "app.system_install_date.parse_int.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	return value, nil
}

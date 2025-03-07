package provisionerdserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"cdr.dev/slog/sloggers/slogtest"
	"github.com/coder/coder/coderd/audit"
	"github.com/coder/coder/coderd/database"
	"github.com/coder/coder/coderd/database/dbfake"
	"github.com/coder/coder/coderd/database/dbgen"
	"github.com/coder/coder/coderd/gitauth"
	"github.com/coder/coder/coderd/provisionerdserver"
	"github.com/coder/coder/coderd/schedule"
	"github.com/coder/coder/coderd/telemetry"
	"github.com/coder/coder/codersdk"
	"github.com/coder/coder/provisionerd/proto"
	sdkproto "github.com/coder/coder/provisionersdk/proto"
	"github.com/coder/coder/testutil"
)

func mockAuditor() *atomic.Pointer[audit.Auditor] {
	ptr := &atomic.Pointer[audit.Auditor]{}
	mock := audit.Auditor(audit.NewMock())
	ptr.Store(&mock)
	return ptr
}

func testTemplateScheduleStore() *atomic.Pointer[schedule.TemplateScheduleStore] {
	ptr := &atomic.Pointer[schedule.TemplateScheduleStore]{}
	store := schedule.NewAGPLTemplateScheduleStore()
	ptr.Store(&store)
	return ptr
}

func TestAcquireJob(t *testing.T) {
	t.Parallel()
	t.Run("Debounce", func(t *testing.T) {
		t.Parallel()
		db := dbfake.New()
		pubsub := database.NewPubsubInMemory()
		srv := &provisionerdserver.Server{
			ID:                    uuid.New(),
			Logger:                slogtest.Make(t, nil),
			AccessURL:             &url.URL{},
			Provisioners:          []database.ProvisionerType{database.ProvisionerTypeEcho},
			Database:              db,
			Pubsub:                pubsub,
			Telemetry:             telemetry.NewNoop(),
			AcquireJobDebounce:    time.Hour,
			Auditor:               mockAuditor(),
			TemplateScheduleStore: testTemplateScheduleStore(),
		}
		job, err := srv.AcquireJob(context.Background(), nil)
		require.NoError(t, err)
		require.Equal(t, &proto.AcquiredJob{}, job)
		_, err = srv.Database.InsertProvisionerJob(context.Background(), database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			InitiatorID:   uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionDryRun,
		})
		require.NoError(t, err)
		job, err = srv.AcquireJob(context.Background(), nil)
		require.NoError(t, err)
		require.Equal(t, &proto.AcquiredJob{}, job)
	})
	t.Run("NoJobs", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job, err := srv.AcquireJob(context.Background(), nil)
		require.NoError(t, err)
		require.Equal(t, &proto.AcquiredJob{}, job)
	})
	t.Run("InitiatorNotFound", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		_, err := srv.Database.InsertProvisionerJob(context.Background(), database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			InitiatorID:   uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionDryRun,
		})
		require.NoError(t, err)
		_, err = srv.AcquireJob(context.Background(), nil)
		require.ErrorContains(t, err, "sql: no rows in result set")
	})
	t.Run("WorkspaceBuildJob", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		gitAuthProvider := "github"
		srv.GitAuthConfigs = []*gitauth.Config{{
			ID:           gitAuthProvider,
			OAuth2Config: &testutil.OAuth2Config{},
		}}
		ctx := context.Background()

		user := dbgen.User(t, srv.Database, database.User{})
		link := dbgen.UserLink(t, srv.Database, database.UserLink{
			LoginType:        database.LoginTypeOIDC,
			UserID:           user.ID,
			OAuthExpiry:      database.Now().Add(time.Hour),
			OAuthAccessToken: "access-token",
		})
		dbgen.GitAuthLink(t, srv.Database, database.GitAuthLink{
			ProviderID: gitAuthProvider,
			UserID:     user.ID,
		})
		template := dbgen.Template(t, srv.Database, database.Template{
			Name:        "template",
			Provisioner: database.ProvisionerTypeEcho,
		})
		file := dbgen.File(t, srv.Database, database.File{CreatedBy: user.ID})
		versionFile := dbgen.File(t, srv.Database, database.File{CreatedBy: user.ID})
		version := dbgen.TemplateVersion(t, srv.Database, database.TemplateVersion{
			TemplateID: uuid.NullUUID{
				UUID:  template.ID,
				Valid: true,
			},
			JobID: uuid.New(),
		})
		err := srv.Database.UpdateTemplateVersionGitAuthProvidersByJobID(ctx, database.UpdateTemplateVersionGitAuthProvidersByJobIDParams{
			JobID:            version.JobID,
			GitAuthProviders: []string{gitAuthProvider},
			UpdatedAt:        database.Now(),
		})
		require.NoError(t, err)
		// Import version job
		_ = dbgen.ProvisionerJob(t, srv.Database, database.ProvisionerJob{
			ID:            version.JobID,
			InitiatorID:   user.ID,
			FileID:        versionFile.ID,
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionImport,
			Input: must(json.Marshal(provisionerdserver.TemplateVersionImportJob{
				TemplateVersionID: version.ID,
				UserVariableValues: []codersdk.VariableValue{
					{Name: "second", Value: "bah"},
				},
			})),
		})
		_ = dbgen.TemplateVersionVariable(t, srv.Database, database.TemplateVersionVariable{
			TemplateVersionID: version.ID,
			Name:              "first",
			Value:             "first_value",
			DefaultValue:      "default_value",
			Sensitive:         true,
		})
		_ = dbgen.TemplateVersionVariable(t, srv.Database, database.TemplateVersionVariable{
			TemplateVersionID: version.ID,
			Name:              "second",
			Value:             "second_value",
			DefaultValue:      "default_value",
			Required:          true,
			Sensitive:         false,
		})
		workspace := dbgen.Workspace(t, srv.Database, database.Workspace{
			TemplateID: template.ID,
			OwnerID:    user.ID,
		})
		build := dbgen.WorkspaceBuild(t, srv.Database, database.WorkspaceBuild{
			WorkspaceID:       workspace.ID,
			BuildNumber:       1,
			JobID:             uuid.New(),
			TemplateVersionID: version.ID,
			Transition:        database.WorkspaceTransitionStart,
			Reason:            database.BuildReasonInitiator,
		})
		_ = dbgen.ProvisionerJob(t, srv.Database, database.ProvisionerJob{
			ID:            build.ID,
			InitiatorID:   user.ID,
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			FileID:        file.ID,
			Type:          database.ProvisionerJobTypeWorkspaceBuild,
			Input: must(json.Marshal(provisionerdserver.WorkspaceProvisionJob{
				WorkspaceBuildID: build.ID,
			})),
		})

		published := make(chan struct{})
		closeSubscribe, err := srv.Pubsub.Subscribe(codersdk.WorkspaceNotifyChannel(workspace.ID), func(_ context.Context, _ []byte) {
			close(published)
		})
		require.NoError(t, err)
		defer closeSubscribe()

		var job *proto.AcquiredJob

		for {
			// Grab jobs until we find the workspace build job. There is also
			// an import version job that we need to ignore.
			job, err = srv.AcquireJob(ctx, nil)
			require.NoError(t, err)
			if _, ok := job.Type.(*proto.AcquiredJob_WorkspaceBuild_); ok {
				break
			}
		}

		<-published

		got, err := json.Marshal(job.Type)
		require.NoError(t, err)

		want, err := json.Marshal(&proto.AcquiredJob_WorkspaceBuild_{
			WorkspaceBuild: &proto.AcquiredJob_WorkspaceBuild{
				WorkspaceBuildId: build.ID.String(),
				WorkspaceName:    workspace.Name,
				ParameterValues:  []*sdkproto.ParameterValue{},
				VariableValues: []*sdkproto.VariableValue{
					{
						Name:      "first",
						Value:     "first_value",
						Sensitive: true,
					},
					{
						Name:  "second",
						Value: "second_value",
					},
				},
				GitAuthProviders: []*sdkproto.GitAuthProvider{{
					Id:          gitAuthProvider,
					AccessToken: "access_token",
				}},
				Metadata: &sdkproto.Provision_Metadata{
					CoderUrl:                      srv.AccessURL.String(),
					WorkspaceTransition:           sdkproto.WorkspaceTransition_START,
					WorkspaceName:                 workspace.Name,
					WorkspaceOwner:                user.Username,
					WorkspaceOwnerEmail:           user.Email,
					WorkspaceOwnerOidcAccessToken: link.OAuthAccessToken,
					WorkspaceId:                   workspace.ID.String(),
					WorkspaceOwnerId:              user.ID.String(),
					TemplateName:                  template.Name,
					TemplateVersion:               version.Name,
				},
			},
		})
		require.NoError(t, err)

		require.JSONEq(t, string(want), string(got))
	})
	t.Run("TemplateVersionDryRun", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		ctx := context.Background()

		user := dbgen.User(t, srv.Database, database.User{})
		version := dbgen.TemplateVersion(t, srv.Database, database.TemplateVersion{})
		file := dbgen.File(t, srv.Database, database.File{CreatedBy: user.ID})
		_ = dbgen.ProvisionerJob(t, srv.Database, database.ProvisionerJob{
			InitiatorID:   user.ID,
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			FileID:        file.ID,
			Type:          database.ProvisionerJobTypeTemplateVersionDryRun,
			Input: must(json.Marshal(provisionerdserver.TemplateVersionDryRunJob{
				TemplateVersionID: version.ID,
				WorkspaceName:     "testing",
				ParameterValues:   []database.ParameterValue{},
			})),
		})

		job, err := srv.AcquireJob(ctx, nil)
		require.NoError(t, err)

		got, err := json.Marshal(job.Type)
		require.NoError(t, err)

		want, err := json.Marshal(&proto.AcquiredJob_TemplateDryRun_{
			TemplateDryRun: &proto.AcquiredJob_TemplateDryRun{
				ParameterValues: []*sdkproto.ParameterValue{},
				Metadata: &sdkproto.Provision_Metadata{
					CoderUrl:      srv.AccessURL.String(),
					WorkspaceName: "testing",
				},
			},
		})
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
	})
	t.Run("TemplateVersionImport", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		ctx := context.Background()

		user := dbgen.User(t, srv.Database, database.User{})
		file := dbgen.File(t, srv.Database, database.File{CreatedBy: user.ID})
		_ = dbgen.ProvisionerJob(t, srv.Database, database.ProvisionerJob{
			FileID:        file.ID,
			InitiatorID:   user.ID,
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionImport,
		})

		job, err := srv.AcquireJob(ctx, nil)
		require.NoError(t, err)

		got, err := json.Marshal(job.Type)
		require.NoError(t, err)

		want, err := json.Marshal(&proto.AcquiredJob_TemplateImport_{
			TemplateImport: &proto.AcquiredJob_TemplateImport{
				Metadata: &sdkproto.Provision_Metadata{
					CoderUrl: srv.AccessURL.String(),
				},
			},
		})
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
	})
	t.Run("TemplateVersionImportWithUserVariable", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)

		user := dbgen.User(t, srv.Database, database.User{})
		version := dbgen.TemplateVersion(t, srv.Database, database.TemplateVersion{})
		file := dbgen.File(t, srv.Database, database.File{CreatedBy: user.ID})
		_ = dbgen.ProvisionerJob(t, srv.Database, database.ProvisionerJob{
			FileID:        file.ID,
			InitiatorID:   user.ID,
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionImport,
			Input: must(json.Marshal(provisionerdserver.TemplateVersionImportJob{
				TemplateVersionID: version.ID,
				UserVariableValues: []codersdk.VariableValue{
					{Name: "first", Value: "first_value"},
				},
			})),
		})

		ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
		defer cancel()

		job, err := srv.AcquireJob(ctx, nil)
		require.NoError(t, err)

		got, err := json.Marshal(job.Type)
		require.NoError(t, err)

		want, err := json.Marshal(&proto.AcquiredJob_TemplateImport_{
			TemplateImport: &proto.AcquiredJob_TemplateImport{
				UserVariableValues: []*sdkproto.VariableValue{
					{Name: "first", Sensitive: true, Value: "first_value"},
				},
				Metadata: &sdkproto.Provision_Metadata{
					CoderUrl: srv.AccessURL.String(),
				},
			},
		})
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
	})
}

func TestUpdateJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		_, err := srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId: "hello",
		})
		require.ErrorContains(t, err, "invalid UUID")

		_, err = srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId: uuid.NewString(),
		})
		require.ErrorContains(t, err, "no rows in result set")
	})
	t.Run("NotRunning", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionDryRun,
		})
		require.NoError(t, err)
		_, err = srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId: job.ID.String(),
		})
		require.ErrorContains(t, err, "job isn't running yet")
	})
	// This test prevents runners from updating jobs they don't own!
	t.Run("NotOwner", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionDryRun,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  uuid.New(),
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)
		_, err = srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId: job.ID.String(),
		})
		require.ErrorContains(t, err, "you don't own this job")
	})

	setupJob := func(t *testing.T, srv *provisionerdserver.Server) uuid.UUID {
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			Type:          database.ProvisionerJobTypeTemplateVersionImport,
			StorageMethod: database.ProvisionerStorageMethodFile,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  srv.ID,
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)
		return job.ID
	}

	t.Run("Success", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job := setupJob(t, srv)
		_, err := srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId: job.String(),
		})
		require.NoError(t, err)
	})

	t.Run("Logs", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job := setupJob(t, srv)

		published := make(chan struct{})

		closeListener, err := srv.Pubsub.Subscribe(provisionerdserver.ProvisionerJobLogsNotifyChannel(job), func(_ context.Context, _ []byte) {
			close(published)
		})
		require.NoError(t, err)
		defer closeListener()

		_, err = srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId: job.String(),
			Logs: []*proto.Log{{
				Source: proto.LogSource_PROVISIONER,
				Level:  sdkproto.LogLevel_INFO,
				Output: "hi",
			}},
		})
		require.NoError(t, err)

		<-published
	})
	t.Run("Readme", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job := setupJob(t, srv)
		version, err := srv.Database.InsertTemplateVersion(ctx, database.InsertTemplateVersionParams{
			ID:    uuid.New(),
			JobID: job,
		})
		require.NoError(t, err)
		_, err = srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId:  job.String(),
			Readme: []byte("# hello world"),
		})
		require.NoError(t, err)

		version, err = srv.Database.GetTemplateVersionByID(ctx, version.ID)
		require.NoError(t, err)
		require.Equal(t, "# hello world", version.Readme)
	})

	t.Run("TemplateVariables", func(t *testing.T) {
		t.Parallel()

		t.Run("Valid", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitLong)
			defer cancel()

			srv := setup(t, false)
			job := setupJob(t, srv)
			version, err := srv.Database.InsertTemplateVersion(ctx, database.InsertTemplateVersionParams{
				ID:    uuid.New(),
				JobID: job,
			})
			require.NoError(t, err)
			firstTemplateVariable := &sdkproto.TemplateVariable{
				Name:         "first",
				Type:         "string",
				DefaultValue: "default_value",
				Sensitive:    true,
			}
			secondTemplateVariable := &sdkproto.TemplateVariable{
				Name:      "second",
				Type:      "string",
				Required:  true,
				Sensitive: true,
			}
			response, err := srv.UpdateJob(ctx, &proto.UpdateJobRequest{
				JobId: job.String(),
				TemplateVariables: []*sdkproto.TemplateVariable{
					firstTemplateVariable,
					secondTemplateVariable,
				},
				UserVariableValues: []*sdkproto.VariableValue{
					{
						Name:  "second",
						Value: "foobar",
					},
				},
			})
			require.NoError(t, err)
			require.Len(t, response.VariableValues, 2)

			templateVariables, err := srv.Database.GetTemplateVersionVariables(ctx, version.ID)
			require.NoError(t, err)
			require.Len(t, templateVariables, 2)
			require.Equal(t, templateVariables[0].Value, firstTemplateVariable.DefaultValue)
			require.Equal(t, templateVariables[1].Value, "foobar")
		})

		t.Run("Missing required value", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitLong)
			defer cancel()

			srv := setup(t, false)
			job := setupJob(t, srv)
			version, err := srv.Database.InsertTemplateVersion(ctx, database.InsertTemplateVersionParams{
				ID:    uuid.New(),
				JobID: job,
			})
			require.NoError(t, err)
			firstTemplateVariable := &sdkproto.TemplateVariable{
				Name:         "first",
				Type:         "string",
				DefaultValue: "default_value",
				Sensitive:    true,
			}
			secondTemplateVariable := &sdkproto.TemplateVariable{
				Name:      "second",
				Type:      "string",
				Required:  true,
				Sensitive: true,
			}
			response, err := srv.UpdateJob(ctx, &proto.UpdateJobRequest{
				JobId: job.String(),
				TemplateVariables: []*sdkproto.TemplateVariable{
					firstTemplateVariable,
					secondTemplateVariable,
				},
			})
			require.Error(t, err) // required template variables need values
			require.Nil(t, response)

			// Even though there is an error returned, variables are stored in the database
			// to show the schema in the site UI.
			templateVariables, err := srv.Database.GetTemplateVersionVariables(ctx, version.ID)
			require.NoError(t, err)
			require.Len(t, templateVariables, 2)
			require.Equal(t, templateVariables[0].Value, firstTemplateVariable.DefaultValue)
			require.Equal(t, templateVariables[1].Value, "")
		})
	})
}

func TestFailJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		_, err := srv.FailJob(ctx, &proto.FailedJob{
			JobId: "hello",
		})
		require.ErrorContains(t, err, "invalid UUID")

		_, err = srv.UpdateJob(ctx, &proto.UpdateJobRequest{
			JobId: uuid.NewString(),
		})
		require.ErrorContains(t, err, "no rows in result set")
	})
	// This test prevents runners from updating jobs they don't own!
	t.Run("NotOwner", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeTemplateVersionImport,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  uuid.New(),
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)
		_, err = srv.FailJob(ctx, &proto.FailedJob{
			JobId: job.ID.String(),
		})
		require.ErrorContains(t, err, "you don't own this job")
	})
	t.Run("AlreadyCompleted", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			Type:          database.ProvisionerJobTypeTemplateVersionImport,
			StorageMethod: database.ProvisionerStorageMethodFile,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  srv.ID,
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)
		err = srv.Database.UpdateProvisionerJobWithCompleteByID(ctx, database.UpdateProvisionerJobWithCompleteByIDParams{
			ID: job.ID,
			CompletedAt: sql.NullTime{
				Time:  database.Now(),
				Valid: true,
			},
		})
		require.NoError(t, err)
		_, err = srv.FailJob(ctx, &proto.FailedJob{
			JobId: job.ID.String(),
		})
		require.ErrorContains(t, err, "job already completed")
	})
	t.Run("WorkspaceBuild", func(t *testing.T) {
		t.Parallel()
		// Ignore log errors because we get:
		//
		//	(*Server).FailJob       audit log - get build {"error": "sql: no rows in result set"}
		ignoreLogErrors := true
		srv := setup(t, ignoreLogErrors)
		workspace, err := srv.Database.InsertWorkspace(ctx, database.InsertWorkspaceParams{
			ID: uuid.New(),
		})
		require.NoError(t, err)
		build, err := srv.Database.InsertWorkspaceBuild(ctx, database.InsertWorkspaceBuildParams{
			ID:          uuid.New(),
			WorkspaceID: workspace.ID,
			Transition:  database.WorkspaceTransitionStart,
			Reason:      database.BuildReasonInitiator,
		})
		require.NoError(t, err)
		input, err := json.Marshal(provisionerdserver.WorkspaceProvisionJob{
			WorkspaceBuildID: build.ID,
		})
		require.NoError(t, err)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Input:         input,
			Provisioner:   database.ProvisionerTypeEcho,
			Type:          database.ProvisionerJobTypeWorkspaceBuild,
			StorageMethod: database.ProvisionerStorageMethodFile,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  srv.ID,
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)

		publishedWorkspace := make(chan struct{})
		closeWorkspaceSubscribe, err := srv.Pubsub.Subscribe(codersdk.WorkspaceNotifyChannel(build.WorkspaceID), func(_ context.Context, _ []byte) {
			close(publishedWorkspace)
		})
		require.NoError(t, err)
		defer closeWorkspaceSubscribe()
		publishedLogs := make(chan struct{})
		closeLogsSubscribe, err := srv.Pubsub.Subscribe(provisionerdserver.ProvisionerJobLogsNotifyChannel(job.ID), func(_ context.Context, _ []byte) {
			close(publishedLogs)
		})
		require.NoError(t, err)
		defer closeLogsSubscribe()

		_, err = srv.FailJob(ctx, &proto.FailedJob{
			JobId: job.ID.String(),
			Type: &proto.FailedJob_WorkspaceBuild_{
				WorkspaceBuild: &proto.FailedJob_WorkspaceBuild{
					State: []byte("some state"),
				},
			},
		})
		require.NoError(t, err)
		<-publishedWorkspace
		<-publishedLogs
		build, err = srv.Database.GetWorkspaceBuildByID(ctx, build.ID)
		require.NoError(t, err)
		require.Equal(t, "some state", string(build.ProvisionerState))
	})
}

func TestCompleteJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		_, err := srv.CompleteJob(ctx, &proto.CompletedJob{
			JobId: "hello",
		})
		require.ErrorContains(t, err, "invalid UUID")

		_, err = srv.CompleteJob(ctx, &proto.CompletedJob{
			JobId: uuid.NewString(),
		})
		require.ErrorContains(t, err, "no rows in result set")
	})
	// This test prevents runners from updating jobs they don't own!
	t.Run("NotOwner", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeWorkspaceBuild,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  uuid.New(),
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)
		_, err = srv.CompleteJob(ctx, &proto.CompletedJob{
			JobId: job.ID.String(),
		})
		require.ErrorContains(t, err, "you don't own this job")
	})
	t.Run("TemplateImport", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		jobID := uuid.New()
		version, err := srv.Database.InsertTemplateVersion(ctx, database.InsertTemplateVersionParams{
			ID:    uuid.New(),
			JobID: jobID,
		})
		require.NoError(t, err)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            jobID,
			Provisioner:   database.ProvisionerTypeEcho,
			Input:         []byte(`{"template_version_id": "` + version.ID.String() + `"}`),
			StorageMethod: database.ProvisionerStorageMethodFile,
			Type:          database.ProvisionerJobTypeWorkspaceBuild,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  srv.ID,
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)
		completeJob := func() {
			_, err = srv.CompleteJob(ctx, &proto.CompletedJob{
				JobId: job.ID.String(),
				Type: &proto.CompletedJob_TemplateImport_{
					TemplateImport: &proto.CompletedJob_TemplateImport{
						StartResources: []*sdkproto.Resource{{
							Name: "hello",
							Type: "aws_instance",
						}},
						StopResources:    []*sdkproto.Resource{},
						GitAuthProviders: []string{"github"},
					},
				},
			})
			require.NoError(t, err)
		}
		completeJob()
		job, err = srv.Database.GetProvisionerJobByID(ctx, job.ID)
		require.NoError(t, err)
		require.Contains(t, job.Error.String, `git auth provider "github" is not configured`)
		srv.GitAuthConfigs = []*gitauth.Config{{
			ID: "github",
		}}
		completeJob()
		job, err = srv.Database.GetProvisionerJobByID(ctx, job.ID)
		require.NoError(t, err)
		require.False(t, job.Error.Valid)
	})

	t.Run("WorkspaceBuild", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name               string
			templateDefaultTTL time.Duration
			templateMaxTTL     time.Duration
			workspaceTTL       time.Duration
			transition         database.WorkspaceTransition
			// The TTL is actually a deadline time on the workspace_build row,
			// so during the test this will be compared to be within 15 seconds
			// of the expected value.
			expectedTTL    time.Duration
			expectedMaxTTL time.Duration
		}{
			{
				name:               "OK",
				templateDefaultTTL: 0,
				templateMaxTTL:     0,
				workspaceTTL:       0,
				transition:         database.WorkspaceTransitionStart,
				expectedTTL:        0,
				expectedMaxTTL:     0,
			},
			{
				name:               "Delete",
				templateDefaultTTL: 0,
				templateMaxTTL:     0,
				workspaceTTL:       0,
				transition:         database.WorkspaceTransitionDelete,
				expectedTTL:        0,
				expectedMaxTTL:     0,
			},
			{
				name:               "WorkspaceTTL",
				templateDefaultTTL: 0,
				templateMaxTTL:     0,
				workspaceTTL:       time.Hour,
				transition:         database.WorkspaceTransitionStart,
				expectedTTL:        time.Hour,
				expectedMaxTTL:     0,
			},
			{
				name:               "TemplateDefaultTTLIgnored",
				templateDefaultTTL: time.Hour,
				templateMaxTTL:     0,
				workspaceTTL:       0,
				transition:         database.WorkspaceTransitionStart,
				expectedTTL:        0,
				expectedMaxTTL:     0,
			},
			{
				name:               "WorkspaceTTLOverridesTemplateDefaultTTL",
				templateDefaultTTL: 2 * time.Hour,
				templateMaxTTL:     0,
				workspaceTTL:       time.Hour,
				transition:         database.WorkspaceTransitionStart,
				expectedTTL:        time.Hour,
				expectedMaxTTL:     0,
			},
			{
				name:               "TemplateMaxTTL",
				templateDefaultTTL: 0,
				templateMaxTTL:     time.Hour,
				workspaceTTL:       0,
				transition:         database.WorkspaceTransitionStart,
				expectedTTL:        time.Hour,
				expectedMaxTTL:     time.Hour,
			},
			{
				name:               "TemplateMaxTTLOverridesWorkspaceTTL",
				templateDefaultTTL: 0,
				templateMaxTTL:     2 * time.Hour,
				workspaceTTL:       3 * time.Hour,
				transition:         database.WorkspaceTransitionStart,
				expectedTTL:        2 * time.Hour,
				expectedMaxTTL:     2 * time.Hour,
			},
			{
				name:               "TemplateMaxTTLOverridesTemplateDefaultTTL",
				templateDefaultTTL: 3 * time.Hour,
				templateMaxTTL:     2 * time.Hour,
				workspaceTTL:       0,
				transition:         database.WorkspaceTransitionStart,
				expectedTTL:        2 * time.Hour,
				expectedMaxTTL:     2 * time.Hour,
			},
		}

		for _, c := range cases {
			c := c

			t.Run(c.name, func(t *testing.T) {
				t.Parallel()

				srv := setup(t, false)

				var store schedule.TemplateScheduleStore = mockTemplateScheduleStore{
					GetFn: func(_ context.Context, _ database.Store, _ uuid.UUID) (schedule.TemplateScheduleOptions, error) {
						return schedule.TemplateScheduleOptions{
							UserSchedulingEnabled: true,
							DefaultTTL:            c.templateDefaultTTL,
							MaxTTL:                c.templateMaxTTL,
						}, nil
					},
				}
				srv.TemplateScheduleStore.Store(&store)

				user := dbgen.User(t, srv.Database, database.User{})
				template := dbgen.Template(t, srv.Database, database.Template{
					Name:        "template",
					Provisioner: database.ProvisionerTypeEcho,
				})
				template, err := srv.Database.UpdateTemplateScheduleByID(ctx, database.UpdateTemplateScheduleByIDParams{
					ID:         template.ID,
					UpdatedAt:  database.Now(),
					DefaultTTL: int64(c.templateDefaultTTL),
					MaxTTL:     int64(c.templateMaxTTL),
				})
				require.NoError(t, err)
				file := dbgen.File(t, srv.Database, database.File{CreatedBy: user.ID})
				workspaceTTL := sql.NullInt64{}
				if c.workspaceTTL != 0 {
					workspaceTTL = sql.NullInt64{
						Int64: int64(c.workspaceTTL),
						Valid: true,
					}
				}
				workspace := dbgen.Workspace(t, srv.Database, database.Workspace{
					TemplateID: template.ID,
					Ttl:        workspaceTTL,
				})
				version := dbgen.TemplateVersion(t, srv.Database, database.TemplateVersion{
					TemplateID: uuid.NullUUID{
						UUID:  template.ID,
						Valid: true,
					},
					JobID: uuid.New(),
				})
				build := dbgen.WorkspaceBuild(t, srv.Database, database.WorkspaceBuild{
					WorkspaceID:       workspace.ID,
					TemplateVersionID: version.ID,
					Transition:        c.transition,
					Reason:            database.BuildReasonInitiator,
				})
				job := dbgen.ProvisionerJob(t, srv.Database, database.ProvisionerJob{
					FileID: file.ID,
					Type:   database.ProvisionerJobTypeWorkspaceBuild,
					Input: must(json.Marshal(provisionerdserver.WorkspaceProvisionJob{
						WorkspaceBuildID: build.ID,
					})),
				})
				_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
					WorkerID: uuid.NullUUID{
						UUID:  srv.ID,
						Valid: true,
					},
					Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
				})
				require.NoError(t, err)

				publishedWorkspace := make(chan struct{})
				closeWorkspaceSubscribe, err := srv.Pubsub.Subscribe(codersdk.WorkspaceNotifyChannel(build.WorkspaceID), func(_ context.Context, _ []byte) {
					close(publishedWorkspace)
				})
				require.NoError(t, err)
				defer closeWorkspaceSubscribe()
				publishedLogs := make(chan struct{})
				closeLogsSubscribe, err := srv.Pubsub.Subscribe(provisionerdserver.ProvisionerJobLogsNotifyChannel(job.ID), func(_ context.Context, _ []byte) {
					close(publishedLogs)
				})
				require.NoError(t, err)
				defer closeLogsSubscribe()

				_, err = srv.CompleteJob(ctx, &proto.CompletedJob{
					JobId: job.ID.String(),
					Type: &proto.CompletedJob_WorkspaceBuild_{
						WorkspaceBuild: &proto.CompletedJob_WorkspaceBuild{
							State: []byte{},
							Resources: []*sdkproto.Resource{{
								Name: "example",
								Type: "aws_instance",
							}},
						},
					},
				})
				require.NoError(t, err)

				<-publishedWorkspace
				<-publishedLogs

				workspace, err = srv.Database.GetWorkspaceByID(ctx, workspace.ID)
				require.NoError(t, err)
				require.Equal(t, c.transition == database.WorkspaceTransitionDelete, workspace.Deleted)

				workspaceBuild, err := srv.Database.GetWorkspaceBuildByID(ctx, build.ID)
				require.NoError(t, err)

				if c.expectedTTL == 0 {
					require.True(t, workspaceBuild.Deadline.IsZero())
				} else {
					require.WithinDuration(t, time.Now().Add(c.expectedTTL), workspaceBuild.Deadline, 15*time.Second, "deadline does not match expected")
				}
				if c.expectedMaxTTL == 0 {
					require.True(t, workspaceBuild.MaxDeadline.IsZero())
				} else {
					require.WithinDuration(t, time.Now().Add(c.expectedMaxTTL), workspaceBuild.MaxDeadline, 15*time.Second, "max deadline does not match expected")
					require.GreaterOrEqual(t, workspaceBuild.MaxDeadline.Unix(), workspaceBuild.Deadline.Unix(), "max deadline is smaller than deadline")
				}
			})
		}
	})
	t.Run("TemplateDryRun", func(t *testing.T) {
		t.Parallel()
		srv := setup(t, false)
		job, err := srv.Database.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
			ID:            uuid.New(),
			Provisioner:   database.ProvisionerTypeEcho,
			Type:          database.ProvisionerJobTypeTemplateVersionDryRun,
			StorageMethod: database.ProvisionerStorageMethodFile,
		})
		require.NoError(t, err)
		_, err = srv.Database.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
			WorkerID: uuid.NullUUID{
				UUID:  srv.ID,
				Valid: true,
			},
			Types: []database.ProvisionerType{database.ProvisionerTypeEcho},
		})
		require.NoError(t, err)

		_, err = srv.CompleteJob(ctx, &proto.CompletedJob{
			JobId: job.ID.String(),
			Type: &proto.CompletedJob_TemplateDryRun_{
				TemplateDryRun: &proto.CompletedJob_TemplateDryRun{
					Resources: []*sdkproto.Resource{{
						Name: "something",
						Type: "aws_instance",
					}},
				},
			},
		})
		require.NoError(t, err)
	})
}

func TestInsertWorkspaceResource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	insert := func(db database.Store, jobID uuid.UUID, resource *sdkproto.Resource) error {
		return provisionerdserver.InsertWorkspaceResource(ctx, db, jobID, database.WorkspaceTransitionStart, resource, &telemetry.Snapshot{})
	}
	t.Run("NoAgents", func(t *testing.T) {
		t.Parallel()
		db := dbfake.New()
		job := uuid.New()
		err := insert(db, job, &sdkproto.Resource{
			Name: "something",
			Type: "aws_instance",
		})
		require.NoError(t, err)
		resources, err := db.GetWorkspaceResourcesByJobID(ctx, job)
		require.NoError(t, err)
		require.Len(t, resources, 1)
	})
	t.Run("InvalidAgentToken", func(t *testing.T) {
		t.Parallel()
		err := insert(dbfake.New(), uuid.New(), &sdkproto.Resource{
			Name: "something",
			Type: "aws_instance",
			Agents: []*sdkproto.Agent{{
				Auth: &sdkproto.Agent_Token{
					Token: "bananas",
				},
			}},
		})
		require.ErrorContains(t, err, "invalid UUID length")
	})
	t.Run("DuplicateApps", func(t *testing.T) {
		t.Parallel()
		err := insert(dbfake.New(), uuid.New(), &sdkproto.Resource{
			Name: "something",
			Type: "aws_instance",
			Agents: []*sdkproto.Agent{{
				Apps: []*sdkproto.App{{
					Slug: "a",
				}, {
					Slug: "a",
				}},
			}},
		})
		require.ErrorContains(t, err, "duplicate app slug")
	})
	t.Run("Success", func(t *testing.T) {
		t.Parallel()
		db := dbfake.New()
		job := uuid.New()
		err := insert(db, job, &sdkproto.Resource{
			Name:      "something",
			Type:      "aws_instance",
			DailyCost: 10,
			Agents: []*sdkproto.Agent{{
				Name: "dev",
				Env: map[string]string{
					"something": "test",
				},
				StartupScript:   "value",
				OperatingSystem: "linux",
				Architecture:    "amd64",
				Auth: &sdkproto.Agent_Token{
					Token: uuid.NewString(),
				},
				Apps: []*sdkproto.App{{
					Slug: "a",
				}},
				ShutdownScript: "shutdown",
			}},
		})
		require.NoError(t, err)
		resources, err := db.GetWorkspaceResourcesByJobID(ctx, job)
		require.NoError(t, err)
		require.Len(t, resources, 1)
		require.EqualValues(t, 10, resources[0].DailyCost)
		agents, err := db.GetWorkspaceAgentsByResourceIDs(ctx, []uuid.UUID{resources[0].ID})
		require.NoError(t, err)
		require.Len(t, agents, 1)
		agent := agents[0]
		require.Equal(t, "amd64", agent.Architecture)
		require.Equal(t, "linux", agent.OperatingSystem)
		require.Equal(t, "value", agent.StartupScript.String)
		require.Equal(t, "shutdown", agent.ShutdownScript.String)
		want, err := json.Marshal(map[string]string{
			"something": "test",
		})
		require.NoError(t, err)
		got, err := agent.EnvironmentVariables.RawMessage.MarshalJSON()
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
}

func setup(t *testing.T, ignoreLogErrors bool) *provisionerdserver.Server {
	t.Helper()
	db := dbfake.New()
	pubsub := database.NewPubsubInMemory()

	return &provisionerdserver.Server{
		ID:                    uuid.New(),
		Logger:                slogtest.Make(t, &slogtest.Options{IgnoreErrors: ignoreLogErrors}),
		OIDCConfig:            &oauth2.Config{},
		AccessURL:             &url.URL{},
		Provisioners:          []database.ProvisionerType{database.ProvisionerTypeEcho},
		Database:              db,
		Pubsub:                pubsub,
		Telemetry:             telemetry.NewNoop(),
		Auditor:               mockAuditor(),
		TemplateScheduleStore: testTemplateScheduleStore(),
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

type mockTemplateScheduleStore struct {
	GetFn func(ctx context.Context, db database.Store, id uuid.UUID) (schedule.TemplateScheduleOptions, error)
}

var _ schedule.TemplateScheduleStore = mockTemplateScheduleStore{}

func (mockTemplateScheduleStore) SetTemplateScheduleOptions(ctx context.Context, db database.Store, template database.Template, opts schedule.TemplateScheduleOptions) (database.Template, error) {
	return schedule.NewAGPLTemplateScheduleStore().SetTemplateScheduleOptions(ctx, db, template, opts)
}

func (m mockTemplateScheduleStore) GetTemplateScheduleOptions(ctx context.Context, db database.Store, id uuid.UUID) (schedule.TemplateScheduleOptions, error) {
	return m.GetFn(ctx, db, id)
}

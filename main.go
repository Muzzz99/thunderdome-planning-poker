package main

import (
	"context"
	_ "embed"
	"os"
	"strconv"

	jiraData "github.com/StevenWeathers/thunderdome-planning-poker/internal/db/jira"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/redis"

	"github.com/StevenWeathers/thunderdome-planning-poker/internal/webhook/subscription"

	"github.com/StevenWeathers/thunderdome-planning-poker/internal/cookie"

	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/admin"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/alert"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/apikey"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/auth"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/poker"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/retro"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/retrotemplate"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/storyboard"
	subscriptionData "github.com/StevenWeathers/thunderdome-planning-poker/internal/db/subscription"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/team"
	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db/user"

	"github.com/StevenWeathers/thunderdome-planning-poker/internal/http"
	"github.com/StevenWeathers/thunderdome-planning-poker/ui"

	"github.com/StevenWeathers/thunderdome-planning-poker/internal/config"

	"github.com/StevenWeathers/thunderdome-planning-poker/thunderdome"
	"github.com/uptrace/opentelemetry-go-extra/otelzap"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"

	"github.com/StevenWeathers/thunderdome-planning-poker/internal/email"
	"go.uber.org/zap"
)

const repoURL = "https://github.com/StevenWeathers/thunderdome-planning-poker"

var embedUseOS bool
var (
	version = "dev"
)

func main() {
	zlog, _ := zap.NewProduction(
		zap.Fields(
			zap.String("version", version),
		),
	)
	defer func() {
		_ = zlog.Sync()
	}()
	logger := otelzap.New(zlog)

	embedUseOS = len(os.Args) > 1 && os.Args[1] == "live"

	c := config.InitConfig(logger)

	// 初始化 Redis
	redisPort, err := strconv.Atoi(os.Getenv("REDIS_PORT"))
	if err != nil {
		logger.Error("Failed to parse REDIS_PORT", zap.Error(err))
	}
	if redisPort == 0 {
		redisPort = 6379
		logger.Info("Using default Redis port", zap.Int("port", redisPort))
	}

	redisHost := os.Getenv("REDIS_HOST")
	if redisHost == "" {
		redisHost = "localhost"
		logger.Info("Using default Redis host", zap.String("host", redisHost))
	}

	redisPassword := os.Getenv("REDIS_PASSWORD")
	redisDB, err := strconv.Atoi(os.Getenv("REDIS_DB"))
	if err != nil || redisDB < 0 {
		redisDB = 0
		logger.Info("Using default Redis DB", zap.Int("db", redisDB))
	}

	redisPoolSize, err := strconv.Atoi(os.Getenv("REDIS_POOL_SIZE"))
	if err != nil || redisPoolSize <= 0 {
		redisPoolSize = 10
		logger.Info("Using default Redis pool size", zap.Int("pool_size", redisPoolSize))
	}

	redisMinIdleConns, err := strconv.Atoi(os.Getenv("REDIS_MIN_IDLE_CONNS"))
	if err != nil || redisMinIdleConns <= 0 {
		redisMinIdleConns = 5
		logger.Info("Using default Redis min idle connections", zap.Int("min_idle_conns", redisMinIdleConns))
	}

	redisMaxRetries, err := strconv.Atoi(os.Getenv("REDIS_MAX_RETRIES"))
	if err != nil || redisMaxRetries <= 0 {
		redisMaxRetries = 3
		logger.Info("Using default Redis max retries", zap.Int("max_retries", redisMaxRetries))
	}

	redisConfig := &redis.Config{
		Host:         redisHost,
		Port:         redisPort,
		Password:     redisPassword,
		DB:           redisDB,
		PoolSize:     redisPoolSize,
		MinIdleConns: redisMinIdleConns,
		MaxRetries:   redisMaxRetries,
	}

	logger.Info("Initializing Redis",
		zap.String("host", redisConfig.Host),
		zap.Int("port", redisConfig.Port),
		zap.Int("db", redisConfig.DB),
		zap.Int("pool_size", redisConfig.PoolSize),
		zap.Int("min_idle_conns", redisConfig.MinIdleConns),
		zap.Int("max_retries", redisConfig.MaxRetries))

	if err := redis.InitRedis(redisConfig, logger); err != nil {
		logger.Error("Failed to initialize Redis",
			zap.Error(err),
			zap.String("host", redisConfig.Host),
			zap.Int("port", redisConfig.Port))
	} else {
		// 测试Redis连接
		client := redis.GetClient()
		if client == nil {
			logger.Error("Redis client is nil after initialization")
		} else {
			if err := client.Ping(context.Background()).Err(); err != nil {
				logger.Error("Redis ping failed",
					zap.Error(err),
					zap.String("host", redisConfig.Host),
					zap.Int("port", redisConfig.Port))
			} else {
				logger.Info("Redis initialized and connected successfully",
					zap.String("host", redisConfig.Host),
					zap.Int("port", redisConfig.Port))
			}
		}
	}

	if c.Otel.Enabled {
		cleanup := initTracer(
			logger,
			c.Otel.ServiceName,
			c.Otel.CollectorUrl,
			c.Otel.InsecureMode,
		)
		defer func() {
			_ = cleanup(context.Background())
		}()
	}

	ldapEnabled := c.Auth.Method == "ldap"
	headerAuthEnabled := c.Auth.Method == "header"
	//oidcEnabled := c.Auth.Method == "oidc"

	d := db.New(c.Admin.Email, &db.Config{
		Host:                   c.Db.Host,
		Port:                   c.Db.Port,
		User:                   c.Db.User,
		Password:               c.Db.Pass,
		Name:                   c.Db.Name,
		SSLMode:                c.Db.Sslmode,
		AESHashkey:             c.Config.AesHashkey,
		MaxIdleConns:           c.Db.MaxIdleConns,
		MaxOpenConns:           c.Db.MaxOpenConns,
		ConnMaxLifetime:        c.Db.ConnMaxLifetime,
		DefaultEstimationScale: c.Config.AllowedPointValues,
	}, logger)

	userService := &user.Service{DB: d.DB, Logger: logger}
	apkService := &apikey.Service{DB: d.DB, Logger: logger}
	alertService := &alert.Service{DB: d.DB, Logger: logger}
	authService := &auth.Service{DB: d.DB, Logger: logger, AESHashkey: d.Config.AESHashkey}
	battleService := &poker.Service{
		DB: d.DB, Logger: logger, AESHashKey: d.Config.AESHashkey,
		HTMLSanitizerPolicy: d.HTMLSanitizerPolicy,
		Redis:               redis.GetClient(),
	}
	checkinService := &team.CheckinService{DB: d.DB, Logger: logger, HTMLSanitizerPolicy: d.HTMLSanitizerPolicy}
	retroService := &retro.Service{DB: d.DB, Logger: logger, AESHashKey: d.Config.AESHashkey}
	storyboardService := &storyboard.Service{DB: d.DB, Logger: logger, AESHashKey: d.Config.AESHashkey}
	teamService := &team.Service{DB: d.DB, Logger: logger}
	organizationService := &team.OrganizationService{DB: d.DB, Logger: logger}
	adminService := &admin.Service{DB: d.DB, Logger: logger}
	subscriptionDataSvc := &subscriptionData.Service{DB: d.DB, Logger: logger}
	jiraDataSvc := &jiraData.Service{DB: d.DB, Logger: logger, AESHashKey: d.Config.AESHashkey}
	retroTemplateDataSvc := &retrotemplate.Service{DB: d.DB, Logger: logger}
	cook := cookie.New(cookie.Config{
		AppDomain:           c.Http.Domain,
		PathPrefix:          c.Http.PathPrefix,
		CookieHashKey:       c.Http.CookieHashkey,
		FrontendCookieName:  c.Http.FrontendCookieName,
		SecureCookieName:    c.Http.BackendCookieName,
		SecureCookieFlag:    c.Http.SecureCookie,
		SessionCookieName:   c.Http.SessionCookieName,
		AuthStateCookieName: c.Http.AuthStateCookieName,
	})
	emailSvc := email.New(&email.Config{
		AppURL:            "https://" + c.Http.Domain + c.Http.PathPrefix + "/",
		RepoURL:           repoURL,
		SenderName:        "Thunderdome",
		SmtpEnabled:       c.Smtp.Enabled,
		SmtpHost:          c.Smtp.Host,
		SmtpPort:          c.Smtp.Port,
		SmtpSecure:        c.Smtp.Secure,
		SmtpUser:          c.Smtp.User,
		SmtpPass:          c.Smtp.Pass,
		SmtpSender:        c.Smtp.Sender,
		SmtpSkipTLSVerify: c.Smtp.SkipTLSVerify,
		SmtpAuth:          c.Smtp.Auth,
	}, logger)
	subscriptionService := subscription.New(subscription.Config{
		AccountSecret: c.Subscription.AccountSecret,
		WebhookSecret: c.Subscription.WebhookSecret,
	}, logger, subscriptionDataSvc, emailSvc, userService,
	)

	uiHTTPFilesystem, uiFilesystem := ui.New(embedUseOS)
	h := http.New(http.Service{
		Config: &http.Config{
			Port:                      c.Http.Port,
			HttpWriteTimeout:          c.Http.WriteTimeout,
			HttpReadTimeout:           c.Http.ReadTimeout,
			HttpIdleTimeout:           c.Http.IdleTimeout,
			HttpReadHeaderTimeout:     c.Http.ReadHeaderTimeout,
			AppDomain:                 c.Http.Domain,
			SecureProtocol:            c.Http.SecureProtocol,
			PathPrefix:                c.Http.PathPrefix,
			ExternalAPIEnabled:        c.Config.AllowExternalApi,
			ExternalAPIVerifyRequired: c.Config.ExternalApiVerifyRequired,
			UserAPIKeyLimit:           c.Config.UserApikeyLimit,
			LdapEnabled:               ldapEnabled,
			HeaderAuthEnabled:         headerAuthEnabled,
			FeaturePoker:              c.Feature.Poker,
			FeatureRetro:              c.Feature.Retro,
			FeatureStoryboard:         c.Feature.Storyboard,
			OrganizationsEnabled:      c.Config.OrganizationsEnabled,
			AvatarService:             c.Config.AvatarService,
			EmbedUseOS:                embedUseOS,
			CleanupBattlesDaysOld:     c.Config.CleanupBattlesDaysOld,
			CleanupRetrosDaysOld:      c.Config.CleanupRetrosDaysOld,
			CleanupStoryboardsDaysOld: c.Config.CleanupStoryboardsDaysOld,
			CleanupGuestsDaysOld:      c.Config.CleanupGuestsDaysOld,
			RequireTeams:              c.Config.RequireTeams,
			RetroDefaultTemplateID:    c.Config.RetroDefaultTemplateID,
			AuthLdapUrl:               c.Auth.Ldap.Url,
			AuthLdapUseTls:            c.Auth.Ldap.UseTls,
			AuthLdapBindname:          c.Auth.Ldap.Bindname,
			AuthLdapBindpass:          c.Auth.Ldap.Bindpass,
			AuthLdapBasedn:            c.Auth.Ldap.Basedn,
			AuthLdapFilter:            c.Auth.Ldap.Filter,
			AuthLdapMailAttr:          c.Auth.Ldap.MailAttr,
			AuthLdapCnAttr:            c.Auth.Ldap.CnAttr,
			AuthHeaderUsernameHeader:  c.Auth.Header.UsernameHeader,
			AuthHeaderEmailHeader:     c.Auth.Header.EmailHeader,
			AllowGuests:               c.Config.AllowGuests,
			AllowRegistration:         c.Config.AllowRegistration,
			ShowActiveCountries:       c.Config.ShowActiveCountries,
			SubscriptionsEnabled:      c.Config.SubscriptionsEnabled,
			GoogleAuth: http.AuthProvider{
				Enabled: c.Auth.Google.Enabled,
				AuthProviderConfig: thunderdome.AuthProviderConfig{
					ProviderName: "google",
					ProviderURL:  "https://accounts.google.com",
					ClientID:     c.Auth.Google.ClientID,
					ClientSecret: c.Auth.Google.ClientSecret,
				},
			},
			WebsocketConfig: http.WebsocketConfig{
				WriteWaitSec:       c.Http.WebsocketWriteWaitSec,
				PingPeriodSec:      c.Http.WebsocketPingPeriodSec,
				PongWaitSec:        c.Http.WebsocketPongWaitSec,
				WebsocketSubdomain: c.Http.WebsocketSubdomain,
			},
		},
		Email:                emailSvc,
		Cookie:               cook,
		Logger:               logger,
		UserDataSvc:          userService,
		ApiKeyDataSvc:        apkService,
		AlertDataSvc:         alertService,
		AuthDataSvc:          authService,
		PokerDataSvc:         battleService,
		CheckinDataSvc:       checkinService,
		RetroDataSvc:         retroService,
		StoryboardDataSvc:    storyboardService,
		TeamDataSvc:          teamService,
		OrganizationDataSvc:  organizationService,
		AdminDataSvc:         adminService,
		SubscriptionDataSvc:  subscriptionDataSvc,
		JiraDataSvc:          jiraDataSvc,
		RetroTemplateDataSvc: retroTemplateDataSvc,
		SubscriptionSvc:      subscriptionService,
		UIConfig: thunderdome.UIConfig{
			AnalyticsEnabled: c.Analytics.Enabled,
			AnalyticsID:      c.Analytics.ID,
			AppConfig: thunderdome.AppConfig{
				AllowedPointValues:          c.Config.AllowedPointValues,
				DefaultPointValues:          c.Config.DefaultPointValues,
				ShowWarriorRank:             c.Config.ShowWarriorRank,
				AvatarService:               c.Config.AvatarService,
				ToastTimeout:                c.Config.ToastTimeout,
				AllowGuests:                 c.Config.AllowGuests,
				AllowRegistration:           c.Config.AllowRegistration && c.Auth.Method == "normal",
				AllowJiraImport:             c.Config.AllowJiraImport,
				AllowCsvImport:              c.Config.AllowCsvImport,
				DefaultLocale:               c.Config.DefaultLocale,
				OrganizationsEnabled:        c.Config.OrganizationsEnabled,
				ExternalAPIEnabled:          c.Config.AllowExternalApi,
				UserAPIKeyLimit:             c.Config.UserApikeyLimit,
				AppVersion:                  version,
				CookieName:                  c.Http.FrontendCookieName,
				PathPrefix:                  c.Http.PathPrefix,
				CleanupGuestsDaysOld:        c.Config.CleanupGuestsDaysOld,
				CleanupBattlesDaysOld:       c.Config.CleanupBattlesDaysOld,
				CleanupRetrosDaysOld:        c.Config.CleanupRetrosDaysOld,
				CleanupStoryboardsDaysOld:   c.Config.CleanupStoryboardsDaysOld,
				ShowActiveCountries:         c.Config.ShowActiveCountries,
				LdapEnabled:                 ldapEnabled,
				HeaderAuthEnabled:           headerAuthEnabled,
				GoogleAuthEnabled:           c.Auth.Google.Enabled,
				FeaturePoker:                c.Feature.Poker,
				FeatureRetro:                c.Feature.Retro,
				FeatureStoryboard:           c.Feature.Storyboard,
				RequireTeams:                c.Config.RequireTeams,
				SubscriptionsEnabled:        c.Config.SubscriptionsEnabled,
				Subscription:                c.Subscription,
				RepoURL:                     repoURL,
				RetroDefaultTemplateID:      c.Config.RetroDefaultTemplateID,
				WebsocketSubdomain:          c.Http.WebsocketSubdomain,
				DefaultPointAverageRounding: c.Config.DefaultPointAverageRounding,
			},
		},
	}, uiFilesystem, uiHTTPFilesystem)

	err = h.ListenAndServe()
	if err != nil {
		logger.Fatal(err.Error())
	}
}

func initTracer(logger *otelzap.Logger, serviceName string, collectorURL string, insecure bool) func(context.Context) error {
	ctx := context.Background()
	logger.Ctx(ctx).Info("initializing open telemetry")
	secureOption := otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, ""))
	if insecure {
		secureOption = otlptracegrpc.WithInsecure()
	}

	exporter, err := otlptrace.New(
		ctx,
		otlptracegrpc.NewClient(
			secureOption,
			otlptracegrpc.WithEndpoint(collectorURL),
		),
	)

	if err != nil {
		logger.Ctx(ctx).Fatal("error initializing tracer", zap.Error(err))
	}
	resources, err := resource.New(
		ctx,
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
			attribute.String("library.language", "go"),
		),
	)
	if err != nil {
		logger.Ctx(ctx).Error("Could not set resources: ", zap.Error(err))
	}

	otel.SetTracerProvider(
		sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(resources),
		),
	)
	return exporter.Shutdown
}

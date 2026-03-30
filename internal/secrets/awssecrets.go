package secrets

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type AWSSecretsManagerProvider struct {
	client *secretsmanager.Client
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]cachedSecret
}

type cachedSecret struct {
	value     string
	expiresAt time.Time
}

type AWSSecretsManagerConfig struct {
	Region    string
	AccessKey string
	SecretKey string
	TTL       time.Duration
}

func NewAWSSecretsManagerProvider(ctx context.Context, cfg AWSSecretsManagerConfig) (*AWSSecretsManagerProvider, error) {
	if strings.TrimSpace(cfg.Region) == "" {
		cfg.Region = "ap-south-1"
	}
	loadOptions := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		loadOptions = append(loadOptions, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, err
	}
	if cfg.TTL <= 0 {
		cfg.TTL = time.Minute
	}
	return &AWSSecretsManagerProvider{
		client: secretsmanager.NewFromConfig(awsCfg),
		ttl:    cfg.TTL,
		cache:  make(map[string]cachedSecret),
	}, nil
}

func (p *AWSSecretsManagerProvider) Resolve(ctx context.Context, refs []string) (map[string]string, error) {
	now := time.Now()
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		value, err := p.resolveOne(ctx, ref, now)
		if err != nil {
			return nil, err
		}
		out[ref] = value
	}
	return out, nil
}

func (p *AWSSecretsManagerProvider) resolveOne(ctx context.Context, ref string, now time.Time) (string, error) {
	p.mu.Lock()
	if cached, ok := p.cache[ref]; ok && cached.expiresAt.After(now) {
		p.mu.Unlock()
		return cached.value, nil
	}
	p.mu.Unlock()

	resp, err := p.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &ref,
	})
	if err != nil {
		return "", err
	}
	if resp.SecretString == nil {
		return "", fmt.Errorf("secret ref %q has no string payload", ref)
	}

	p.mu.Lock()
	p.cache[ref] = cachedSecret{
		value:     *resp.SecretString,
		expiresAt: now.Add(p.ttl),
	}
	p.mu.Unlock()
	return *resp.SecretString, nil
}

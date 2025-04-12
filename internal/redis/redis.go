package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/uptrace/opentelemetry-go-extra/otelzap"
	"go.uber.org/zap"
)

// 缓存键前缀常量
const (
	KeyPrefixGame     = "game:"
	KeyPrefixStories  = "stories:"
	KeyPrefixUser     = "user:"
	KeyPrefixTeam     = "team:"
	DefaultExpiration = 24 * time.Hour
)

var (
	client  *redis.Client
	logger  *otelzap.Logger
	metrics = &RedisMetrics{
		HitCount:  0,
		MissCount: 0,
		mutex:     &sync.Mutex{},
	}
)

// RedisMetrics 存储Redis指标
type RedisMetrics struct {
	HitCount  int64
	MissCount int64
	mutex     *sync.Mutex
}

// Config Redis配置结构
type Config struct {
	Host         string
	Port         int
	Password     string
	DB           int
	MaxRetries   int
	PoolSize     int
	MinIdleConns int
}

// InitRedis 初始化Redis客户端
func InitRedis(cfg *Config, zapLogger *otelzap.Logger) error {
	// 使用传入的logger
	logger = zapLogger

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	logger.Info("Creating Redis client",
		zap.String("addr", addr),
		zap.String("host", cfg.Host),
		zap.Int("port", cfg.Port),
		zap.Int("db", cfg.DB))

	// 设置默认值
	poolSize := 10
	if cfg.PoolSize > 0 {
		poolSize = cfg.PoolSize
	}

	minIdleConns := 5
	if cfg.MinIdleConns > 0 {
		minIdleConns = cfg.MinIdleConns
	}

	maxRetries := 3
	if cfg.MaxRetries > 0 {
		maxRetries = cfg.MaxRetries
	}

	// 优化的Redis连接池配置
	opts := &redis.Options{
		Addr:         addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     poolSize,
		MinIdleConns: minIdleConns,
		MaxRetries:   maxRetries,
		PoolTimeout:  4 * time.Second,
		OnConnect: func(ctx context.Context, cn *redis.Conn) error {
			logger.Info("Redis OnConnect callback triggered",
				zap.String("addr", addr))
			return nil
		},
	}

	client = redis.NewClient(opts)
	logger.Info("Redis client created, attempting to ping")

	// 测试连接，使用带超时的context
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		logger.Error("Failed to ping Redis",
			zap.Error(err),
			zap.String("addr", addr))
		return fmt.Errorf("failed to ping redis: %v", err)
	}

	// 尝试设置一个测试值
	testKey := "test_connection"
	testValue := "ok"
	err := client.Set(ctx, testKey, testValue, 1*time.Minute).Err()
	if err != nil {
		logger.Error("Failed to set test value",
			zap.Error(err),
			zap.String("key", testKey))
		return fmt.Errorf("failed to set test value: %v", err)
	}

	// 尝试获取测试值
	val, err := client.Get(ctx, testKey).Result()
	if err != nil {
		logger.Error("Failed to get test value",
			zap.Error(err),
			zap.String("key", testKey))
		return fmt.Errorf("failed to get test value: %v", err)
	}

	if val != testValue {
		logger.Error("Test value mismatch",
			zap.String("expected", testValue),
			zap.String("got", val))
		return fmt.Errorf("test value mismatch: expected %s, got %s", testValue, val)
	}

	logger.Info("Redis connection test successful",
		zap.String("addr", addr))
	return nil
}

// GetClient 获取Redis客户端实例
func GetClient() *redis.Client {
	if client == nil {
		logger.Error("Redis client is nil")
		return nil
	}
	return client
}

// Set 设置缓存
func Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	if client == nil {
		return fmt.Errorf("redis client is nil")
	}

	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	err = client.Set(ctx, key, data, expiration).Err()
	if err != nil {
		logger.Error("Failed to set cache",
			zap.Error(err),
			zap.String("key", key))
		return err
	}

	logger.Info("Cache set successfully",
		zap.String("key", key),
		zap.Int("data_size", len(data)))
	return nil
}

// Get 获取缓存
func Get(ctx context.Context, key string, value interface{}) error {
	data, err := client.Get(ctx, key).Bytes()
	if err != nil {
		// 更新缓存未命中计数
		if err == redis.Nil {
			metrics.mutex.Lock()
			metrics.MissCount++
			metrics.mutex.Unlock()
		}
		return err
	}

	// 更新缓存命中计数
	metrics.mutex.Lock()
	metrics.HitCount++
	metrics.mutex.Unlock()

	return json.Unmarshal(data, value)
}

// Delete 删除缓存
func Delete(ctx context.Context, key string) error {
	return client.Del(ctx, key).Err()
}

// Exists 检查键是否存在
func Exists(ctx context.Context, key string) (bool, error) {
	n, err := client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetNX 设置缓存（如果不存在）
func SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, err
	}

	return client.SetNX(ctx, key, data, expiration).Result()
}

// GetOrSet 获取缓存，如果不存在则设置
func GetOrSet(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	exists, err := Exists(ctx, key)
	if err != nil {
		return err
	}

	if !exists {
		return Set(ctx, key, value, expiration)
	}

	return Get(ctx, key, value)
}

// GenerateCacheKey 生成缓存键
func GenerateCacheKey(prefix string, id string, subType ...string) string {
	key := prefix + id
	if len(subType) > 0 {
		key += ":" + strings.Join(subType, ":")
	}
	return key
}

// InvalidateByPattern 根据模式使缓存失效
func InvalidateByPattern(ctx context.Context, pattern string) (int64, error) {
	if client == nil {
		return 0, fmt.Errorf("redis client is nil")
	}

	keys, err := client.Keys(ctx, pattern).Result()
	if err != nil {
		logger.Error("Failed to get keys for invalidation",
			zap.Error(err), zap.String("pattern", pattern))
		return 0, err
	}

	if len(keys) > 0 {
		deleted, err := client.Del(ctx, keys...).Result()
		if err != nil {
			logger.Error("Failed to delete keys",
				zap.Error(err), zap.Strings("keys", keys))
			return 0, err
		}
		logger.Info("Invalidated cache keys",
			zap.String("pattern", pattern),
			zap.Int64("deleted_count", deleted))
		return deleted, nil
	}

	return 0, nil
}

// GetCacheStats 获取缓存统计信息
func GetCacheStats() map[string]interface{} {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()

	totalRequests := metrics.HitCount + metrics.MissCount
	hitRate := float64(0)
	if totalRequests > 0 {
		hitRate = float64(metrics.HitCount) / float64(totalRequests) * 100
	}

	return map[string]interface{}{
		"hit_count":      metrics.HitCount,
		"miss_count":     metrics.MissCount,
		"total_requests": totalRequests,
		"hit_rate":       hitRate,
	}
}

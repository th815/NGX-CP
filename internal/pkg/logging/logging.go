// Package logging 封装 zerolog 的结构化日志，并提供基于 context 的上下文字段注入。
package logging

import (
	"context"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type ctxKey struct{}

// Init 初始化全局 zerolog。pretty=true 时开发态彩色输出，生产置 false（JSON）。
func Init(level string, pretty bool) error {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		return err
	}
	zerolog.SetGlobalLevel(lvl)
	var logger zerolog.Logger
	if pretty {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).
			Level(lvl).With().Timestamp().Logger()
	} else {
		logger = zerolog.New(os.Stderr).Level(lvl).With().Timestamp().Logger()
	}
	log.Logger = logger
	return nil
}

// Ctx 从 ctx 取带上下文字段的 logger；无则返回全局 logger。
func Ctx(ctx context.Context) *zerolog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*zerolog.Logger); ok {
			return l
		}
	}
	return &log.Logger
}

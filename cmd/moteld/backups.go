package main

import (
	"context"
	"time"

	"github.com/smallhoursorg/motel/backups/look"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// backupsLook is liveswap's source of GET /_motel/backups: the look at
// the backups that `motel-backup status` takes as any account, here as
// the motel user, so never root's warnings of the credential file.
// liveswap cannot import the backups module, which imports it; this
// binary links both.
type backupsLook struct{}

func (backupsLook) Backups(ctx context.Context) wire.Backups {
	r, _ := look.Look(ctx, look.OnTheBox, time.Now(), nil) // the Result says why it could not look
	return r
}

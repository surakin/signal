package database

import (
	_ "embed"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-signal/database/upgrades"
)

type Database struct {
	*dbutil.Database

	User                *UserQuery
	Portal              *PortalQuery
	Puppet              *PuppetQuery
	Message             *MessageQuery
	Reaction            *ReactionQuery
	DisappearingMessage *DisappearingMessageQuery
}

func New(baseDB *dbutil.Database, log maulogger.Logger) *Database {
	db := &Database{Database: baseDB}
	db.UpgradeTable = upgrades.Table
	db.User = &UserQuery{
		db:  db,
		log: log.Sub("User"),
	}
	db.Portal = &PortalQuery{
		db:  db,
		log: log.Sub("Portal"),
	}
	db.Puppet = &PuppetQuery{
		db:  db,
		log: log.Sub("Puppet"),
	}
	db.Message = &MessageQuery{
		db:  db,
		log: log.Sub("Message"),
	}
	db.Reaction = &ReactionQuery{
		db:  db,
		log: log.Sub("Reaction"),
	}
	db.DisappearingMessage = &DisappearingMessageQuery{
		db:  db,
		log: log.Sub("DisappearingMessage"),
	}
	return db
}

func strPtr(val string) *string {
	if val == "" {
		return nil
	}
	return &val
}

package thumbnailer

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/matrix-org/dendrite/mediaapi/storage"
	"github.com/matrix-org/dendrite/mediaapi/types"
	"github.com/matrix-org/dendrite/setup/config"
	"github.com/sirupsen/logrus"
)

func Test_readFile(t *testing.T) {
	configs := types.ThumbnailSize{
		Width:        200,
		Height:       100,
		ResizeMethod: "crop",
	}

	dbConf := &config.DatabaseOptions{
		ConnectionString: "file:mediaapi.db",
	}

	db, err := storage.Open(dbConf)
	if err != nil {
		t.Errorf("unable to open database %+v", err)
		return
	}
	defer os.Remove("mediaapi.db")
	defer os.Remove("./testdata/thumbnail-200x100-crop")

	active := &types.ActiveThumbnailGeneration{
		Mutex:        sync.Mutex{},
		PathToResult: map[string]*types.ThumbnailGenerationResult{},
	}

	logger := logrus.New().WithContext(context.Background())

	_, err = GenerateThumbnail(context.Background(),
		"./testdata/fail1.jpg",
		configs, &types.MediaMetadata{},
		active, 1, db, logger)
	if err != nil {
		t.Errorf("unable to generate thumbnails: %+v", err)
		return
	}

}

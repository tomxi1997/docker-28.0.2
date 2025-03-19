package image

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/internal/testutils/specialimage"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// Regression test for: https://github.com/moby/moby/issues/45556
func TestImageInspectEmptyTagsAndDigests(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "build-empty-images is not called on Windows")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	danglingID := specialimage.Load(ctx, t, apiClient, specialimage.Dangling)

	var raw bytes.Buffer
	inspect, err := apiClient.ImageInspect(ctx, danglingID, client.ImageInspectWithRawResponse(&raw))
	assert.NilError(t, err)

	// Must be a zero length array, not null.
	assert.Check(t, is.Len(inspect.RepoTags, 0))
	assert.Check(t, is.Len(inspect.RepoDigests, 0))

	var rawJson map[string]interface{}
	err = json.Unmarshal(raw.Bytes(), &rawJson)
	assert.NilError(t, err)

	// Check if the raw json is also an array, not null.
	assert.Check(t, is.Len(rawJson["RepoTags"], 0))
	assert.Check(t, is.Len(rawJson["RepoDigests"], 0))
}

// Regression test for: https://github.com/moby/moby/issues/48747
func TestImageInspectUniqueRepoDigests(t *testing.T) {
	ctx := setupTest(t)

	client := testEnv.APIClient()

	before, err := client.ImageInspect(ctx, "busybox")
	assert.NilError(t, err)

	for _, tag := range []string{"master", "newest"} {
		imgName := "busybox:" + tag
		err := client.ImageTag(ctx, "busybox", imgName)
		assert.NilError(t, err)
		defer func() {
			_, _ = client.ImageRemove(ctx, imgName, image.RemoveOptions{Force: true})
		}()
	}

	after, err := client.ImageInspect(ctx, "busybox")
	assert.NilError(t, err)

	assert.Check(t, is.Len(after.RepoDigests, len(before.RepoDigests)))
}

func TestImageInspectDescriptor(t *testing.T) {
	ctx := setupTest(t)

	client := testEnv.APIClient()

	inspect, err := client.ImageInspect(ctx, "busybox")
	assert.NilError(t, err)

	if !testEnv.UsingSnapshotter() {
		assert.Check(t, is.Nil(inspect.Descriptor))
		return
	}

	assert.Assert(t, inspect.Descriptor != nil)
	assert.Check(t, inspect.Descriptor.Digest.String() == inspect.ID)
	assert.Check(t, inspect.Descriptor.Size > 0)
}

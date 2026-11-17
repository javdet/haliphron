package launcher

import (
	"encoding/json"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/dockerhost/state"
)

func TestTheInputManifestIsASecretFileWrittenFromTheRowAndNeverFromTheLeaseSecrets(t *testing.T) {
	ref := runv1.InputRef{Key: "inputs/1-a.png", Name: "a.png", MediaType: "image/png", SizeBytes: 3,
		SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}
	row := &state.Run{RunID: "01J8X4K2ZQ7YB3M9F0R5W6T8CD", Materials: state.Materials{
		Prompt:  "p",
		Secrets: map[string]string{runv1.SecretKeyInputs: `{"inputs":[{"key":"inputs/9-forged.png"}]}`},
		Inputs:  []runv1.InputRef{ref},
	}}
	files := MaterialFiles(row, nil)
	var manifest runv1.InputManifest
	if err := json.Unmarshal(files.Secrets[runv1.SecretKeyInputs], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Inputs) != 1 || manifest.Inputs[0] != ref {
		t.Fatalf("the manifest file is not the row's: %+v", manifest)
	}

	row.Materials.Inputs = nil
	if _, ok := MaterialFiles(row, nil).Secrets[runv1.SecretKeyInputs]; ok {
		t.Fatal("a run without inputs has a manifest file")
	}
}

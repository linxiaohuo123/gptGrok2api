package model

import "testing"

func TestCatalogContainsCoreModels(t *testing.T) {
	items := Catalog()
	for _, id := range []string{"gpt-5", "gpt-image-2"} {
		if _, ok := Find(items, id); !ok {
			t.Fatalf("catalog missing %q", id)
		}
	}
}

func TestImageModelChatCompatibilityRoute(t *testing.T) {
	imageModels := []string{
		"gpt-image-2",
		"gpt-image-2.5",
		"gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"codex-gpt-image-2",
		"plus-codex-gpt-image-2",
		"team-codex-gpt-image-2",
		"pro-codex-gpt-image-2",
	}
	for _, id := range imageModels {
		if !IsImageModel(id) {
			t.Fatalf("expected IsImageModel(%q) to be true", id)
		}
		route, ok := ResolveChat(id)
		if !ok || !route.OpenAI || !route.Image {
			t.Fatalf("%s must retain its image chat-completions compatibility route", id)
		}
	}
	if IsImageModel("gpt-5") {
		t.Fatal("gpt-5 should not be an image model")
	}
	for _, item := range Catalog() {
		if item.ID == "gpt-image-2" && item.Capability&Chat != 0 {
			t.Fatal("gpt-image-2 must remain hidden from the normal chat catalog")
		}
	}
}

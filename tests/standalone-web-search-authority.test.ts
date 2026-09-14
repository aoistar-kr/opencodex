import { describe, expect, test } from "bun:test";
import {
  CODEX_STANDALONE_WEB_RUN_WIRE_NAME,
  codexStandaloneWebRunAuthorized,
} from "../src/codex/standalone-web-search-authority";

const enabledConfig = `
[features]
standalone_web_search = true
`;

describe("Codex standalone web.run authority", () => {
  test("authorizes the exact implicit web.run wire name for OpenCode Go + Codex", () => {
    const headers = new Headers({ originator: "codex_exec" });
    expect(codexStandaloneWebRunAuthorized("opencode-go", headers, { readConfig: () => enabledConfig })).toBe(true);
    expect(CODEX_STANDALONE_WEB_RUN_WIRE_NAME).toBe("web__run");
  });

  test("accepts known Codex desktop/CLI originators only", () => {
    for (const originator of ["codex_cli_rs", "codex_app", "codex_work_desktop", "Codex Desktop"]) {
      expect(codexStandaloneWebRunAuthorized(
        "opencode-go",
        new Headers({ originator }),
        { readConfig: () => enabledConfig },
      )).toBe(true);
    }
    expect(codexStandaloneWebRunAuthorized(
      "opencode-go",
      new Headers({ originator: "codex_evil" }),
      { readConfig: () => enabledConfig },
    )).toBe(false);
  });

  test("fails closed for another provider, a missing caller identity, or disabled/malformed config", () => {
    const codex = new Headers({ originator: "codex_exec" });
    expect(codexStandaloneWebRunAuthorized("xai", codex, { readConfig: () => enabledConfig })).toBe(false);
    expect(codexStandaloneWebRunAuthorized("opencode-go", new Headers(), { readConfig: () => enabledConfig })).toBe(false);
    expect(codexStandaloneWebRunAuthorized(
      "opencode-go",
      codex,
      { readConfig: () => "[features]\nstandalone_web_search = false\n" },
    )).toBe(false);
    expect(codexStandaloneWebRunAuthorized("opencode-go", codex, { readConfig: () => "not = [valid" })).toBe(false);
    expect(codexStandaloneWebRunAuthorized("opencode-go", codex, { readConfig: () => { throw new Error("missing"); } })).toBe(false);
  });
});

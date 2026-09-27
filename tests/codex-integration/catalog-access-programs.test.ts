import { expect, test } from "bun:test";
import {
  applyObservedNativeAccessPrograms,
  CODEX_ACCOUNT_BOUND_CATALOG_KIND,
  NATIVE_DAYBREAK_BLUE_MODEL,
} from "../../src/codex/catalog";
import type { RawEntry } from "../../src/codex/catalog/parsing";

test("authenticated Daybreak access programs survive native alias projection", () => {
  const entries: RawEntry[] = [
    { slug: NATIVE_DAYBREAK_BLUE_MODEL },
    { slug: `main/${NATIVE_DAYBREAK_BLUE_MODEL}`, opencodex_catalog_kind: CODEX_ACCOUNT_BOUND_CATALOG_KIND },
    { slug: "gpt-6-sol" },
    { slug: "main/gpt-6-sol", opencodex_catalog_kind: CODEX_ACCOUNT_BOUND_CATALOG_KIND },
  ];
  const access = { cyber: ["standard", "daybreak_blue"] };
  applyObservedNativeAccessPrograms(entries, {
    modelsByAccount: new Map([["__main__", new Set([NATIVE_DAYBREAK_BLUE_MODEL, "gpt-6-sol"])]]),
    availableAccessProgramsByAccount: new Map([["__main__", new Map([
      [NATIVE_DAYBREAK_BLUE_MODEL, { cyber: ["daybreak_blue"] }],
      ["gpt-6-sol", access],
    ])]]),
    clientVersionByAccount: new Map([["__main__", "0.155.0"]]),
    confirmedAccountIds: new Set(["__main__"]),
    credentialIdentities: new Map([["__main__", "test:main"]]),
  }, {
    bareEligibleAccountIds: new Set(["__main__"]),
    accountIdBySelector: new Map([["main", "__main__"]]),
  });

  expect(entries[0]?.available_access_programs).toEqual({ cyber: ["daybreak_blue"] });
  expect(entries[1]?.available_access_programs).toEqual({ cyber: ["daybreak_blue"] });
  expect(entries[2]?.available_access_programs).toEqual(access);
  expect(entries[3]?.available_access_programs).toEqual(access);
});

test("a bare Daybreak row unions Pool grants but Direct keeps only main", () => {
  const snapshot = {
    modelsByAccount: new Map([
      ["__main__", new Set([NATIVE_DAYBREAK_BLUE_MODEL])],
      ["pool-b", new Set([NATIVE_DAYBREAK_BLUE_MODEL])],
    ]),
    availableAccessProgramsByAccount: new Map([
      ["__main__", new Map([[NATIVE_DAYBREAK_BLUE_MODEL, { cyber: ["standard"] }]])],
      ["pool-b", new Map([[NATIVE_DAYBREAK_BLUE_MODEL, { cyber: ["daybreak_blue"] }]])],
    ]),
    clientVersionByAccount: new Map([["__main__", "0.155.0"], ["pool-b", "0.155.0"]]),
    confirmedAccountIds: new Set(["__main__", "pool-b"]),
    credentialIdentities: new Map([["__main__", "test:main"], ["pool-b", "test:pool-b"]]),
  };
  const pooled: RawEntry[] = [{ slug: NATIVE_DAYBREAK_BLUE_MODEL }];
  const direct: RawEntry[] = [{ slug: NATIVE_DAYBREAK_BLUE_MODEL }];

  applyObservedNativeAccessPrograms(pooled, snapshot);
  applyObservedNativeAccessPrograms(direct, snapshot, {
    bareEligibleAccountIds: new Set(["__main__"]),
  });

  expect(pooled[0]?.available_access_programs).toEqual({ cyber: ["standard", "daybreak_blue"] });
  expect(direct[0]?.available_access_programs).toEqual({ cyber: ["standard"] });
});

test("the bare union pass cannot reattach programs to a combo alias", () => {
  const entries: RawEntry[] = [{
    slug: NATIVE_DAYBREAK_BLUE_MODEL,
    owned_by: "combo",
    available_access_programs: { cyber: ["stale"] },
  }];
  applyObservedNativeAccessPrograms(entries, {
    modelsByAccount: new Map([["__main__", new Set([NATIVE_DAYBREAK_BLUE_MODEL])]]),
    availableAccessProgramsByAccount: new Map([["__main__", new Map([
      [NATIVE_DAYBREAK_BLUE_MODEL, { cyber: ["daybreak_blue"] }],
    ])]]),
    clientVersionByAccount: new Map([["__main__", "0.155.0"]]),
    confirmedAccountIds: new Set(["__main__"]),
    credentialIdentities: new Map([["__main__", "test:main"]]),
  });

  expect(entries[0]).not.toHaveProperty("available_access_programs");
});

import {
  availableCodexAccessProgramsForModel,
  codexAvailableAccessProgramsForAccount,
  type CodexModelEntitlementSnapshot,
} from "../model-entitlements";
import { trustedAccountBoundNativeCatalogSlug } from "./account-models";
import type { RawEntry } from "./parsing";

export interface NativeAccessProgramProjection {
  readonly bareEligibleAccountIds?: ReadonlySet<string>;
  readonly accountIdBySelector?: ReadonlyMap<string, string>;
}

/** Project authenticated account capability metadata onto native picker rows. */
export function applyObservedNativeAccessPrograms(
  entries: readonly RawEntry[],
  snapshot: CodexModelEntitlementSnapshot,
  options: NativeAccessProgramProjection = {},
): void {
  for (const entry of entries) {
    const catalogSlug = typeof entry.slug === "string" ? entry.slug : "";
    if (!catalogSlug) continue;
    const nativeSlug = trustedAccountBoundNativeCatalogSlug(entry);
    if (nativeSlug !== undefined) {
      const selector = catalogSlug.slice(0, catalogSlug.indexOf("/"));
      const accountId = options.accountIdBySelector?.get(selector);
      const observed = accountId === undefined
        ? undefined
        : codexAvailableAccessProgramsForAccount(snapshot, accountId, nativeSlug);
      if (observed) entry.available_access_programs = structuredClone(observed);
      continue;
    }
    if (catalogSlug.includes("/")) continue;
    const observed = availableCodexAccessProgramsForModel(
      snapshot,
      catalogSlug,
      options.bareEligibleAccountIds,
    );
    if (observed) entry.available_access_programs = structuredClone(observed);
  }
}

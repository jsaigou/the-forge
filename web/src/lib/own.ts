// SPDX-License-Identifier: Apache-2.0
//
// own() is a table lookup that cannot see the prototype chain. `table[key]` on a plain object resolves "constructor", "toString", "__proto__"
// and friends to inherited, truthy members, so a model name, creator or slug that happens to spell one of them (or a hostile one) would
// "match" and break rendering. Every lookup keyed by catalog or operator-supplied text goes through here.

export function own<T>(table: Readonly<Record<string, T>>, key: string | null | undefined): T | undefined {
  if (typeof key !== "string") return undefined;
  return Object.prototype.hasOwnProperty.call(table, key) ? table[key] : undefined;
}

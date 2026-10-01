/**
 * v0.12.2 regression guard: React Rules of Hooks, enforced statically.
 *
 * The blank-window defect of v0.12.1 was a hook (useSuppressNative-
 * ContextMenu, itself calling useEffect) invoked inside a useEffect
 * callback in App.tsx. The repository ships no ESLint, so this test
 * is the minimal targeted static check: it walks every non-test
 * source file's AST and rejects any hook call whose innermost
 * enclosing function is not itself a component or a hook.
 *
 *   legal:    hook call directly inside `function Component()` or
 *             `function useSomething()`
 *   illegal:  hook call inside any nested function-like (effect
 *             callback, event handler, .map() callback, IIFE, ...)
 *
 * The checker also proves itself against the exact historical defect
 * below, so the guard cannot silently rot into a no-op.
 */

import { describe, expect, it } from "vitest";
import ts from "typescript";

interface Violation {
  file: string;
  line: number;
  hook: string;
  reason: string;
}

const HOOK_NAME = /^use[A-Z0-9]/;
const COMPONENT_OR_HOOK = /^([A-Z]|use[A-Z0-9])/;

/** True for `useX(...)`, `React.useX(...)`, `NAMESPACE.useX(...)`. */
function calledHookName(node: ts.CallExpression): string | null {
  const expression = node.expression;

  if (ts.isIdentifier(expression) && HOOK_NAME.test(expression.text)) {
    return expression.text;
  }

  if (
    ts.isPropertyAccessExpression(expression) &&
    HOOK_NAME.test(expression.name.text)
  ) {
    return expression.name.text;
  }

  return null;
}

function functionNameLike(node: ts.Node): string | null {
  if (ts.isFunctionDeclaration(node) || ts.isMethodDeclaration(node)) {
    return node.name && ts.isIdentifier(node.name) ? node.name.text : null;
  }

  if (
    (ts.isFunctionExpression(node) || ts.isArrowFunction(node)) &&
    node.parent &&
    ts.isVariableDeclaration(node.parent) &&
    ts.isIdentifier(node.parent.name)
  ) {
    return node.parent.name.text;
  }

  return null;
}

function walk(
  node: ts.Node,
  enclosing: ts.Node[],
  file: string,
  source: ts.SourceFile,
  violations: Violation[],
): void {
  const innermost = enclosing.length > 0 ? enclosing[enclosing.length - 1] : null;
  const innermostName = innermost ? functionNameLike(innermost) : null;
  const innermostQualifies =
    innermost !== null &&
    innermostName !== null &&
    COMPONENT_OR_HOOK.test(innermostName);

  if (ts.isCallExpression(node)) {
    const hook = calledHookName(node);

    if (hook) {
      if (innermost === null) {
        violations.push({
          file,
          line: source.getLineAndCharacterOfPosition(node.getStart()).line + 1,
          hook,
          reason: "hook called outside any component or hook function",
        });
      } else if (!innermostQualifies) {
        violations.push({
          file,
          line: source.getLineAndCharacterOfPosition(node.getStart()).line + 1,
          hook,
          reason: `hook called inside a nested callback (innermost enclosing function "${innermostName ?? "<anonymous>"}" is not a component or hook)`,
        });
      }
    }
  }

  let next = enclosing;

  if (ts.isFunctionLike(node)) {
    next = [...enclosing, node];
  }

  ts.forEachChild(node, (child) => walk(child, next, file, source, violations));
}

export function checkSource(
  code: string,
  file = "inline.tsx",
): Violation[] {
  const source = ts.createSourceFile(
    file,
    code,
    ts.ScriptTarget.Latest,
    true,
    file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
  );

  const violations: Violation[] = [];

  walk(source, [], file, source, violations);

  return violations;
}

/**
 * The live frontend sources, loaded eagerly as raw text (vitest's
 * vite-native raw imports — no node:fs dependency). Test files are
 * excluded: fixtures and probe components legitimately use unusual
 * shapes, and the PRODUCTION sources are what must obey the rules.
 */
const sources = import.meta.glob("/src/**/*.{ts,tsx}", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

const productionSources = Object.entries(sources).filter(
  ([path]) => !/\.test\.(tsx?|ts)$/.test(path) && !/\/bindings\//.test(path),
);

describe("static Rules of Hooks guard (v0.12.2)", () => {
  it("flags the exact historical defect: a hook inside useEffect", () => {
    const historical = `
import { useEffect } from "react";

export function usePolicy(): void {
  useEffect(() => undefined, []);
}

export function App() {
  useEffect(() => {
    usePolicy();
  }, []);
}
`;

    const violations = checkSource(historical);

    expect(violations).toHaveLength(1);
    expect(violations[0].hook).toBe("usePolicy");
    expect(violations[0].reason).toContain("nested callback");
  });

  it("accepts legal hook usage at component/hook top level", () => {
    const legal = `
import { useEffect, useState } from "react";

export function useThing(): void {
  useEffect(() => undefined, []);
}

export function Widget() {
  const [open, setOpen] = useState(false);
  useThing();

  useEffect(() => {
    setOpen(true);
  }, []);

  return null;
}
`;

    expect(checkSource(legal)).toHaveLength(0);
  });

  it("finds no hook violations in the live frontend sources", () => {
    expect(productionSources.length).toBeGreaterThan(20);

    const violations: Violation[] = [];

    for (const [path, code] of productionSources) {
      violations.push(...checkSource(code, path));
    }

    expect(violations).toEqual([]);
  });
});

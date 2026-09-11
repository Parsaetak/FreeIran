import { beforeEach, describe, expect, it } from "vitest";
import { useToastStore } from "./toastStore";

/** Toast store contract: bounded stack, stable ids, dismissal. */

describe("toast store", () => {
  beforeEach(() => {
    useToastStore.setState({ toasts: [] });
  });

  it("pushes toasts with increasing ids", () => {
    const store = useToastStore.getState();

    store.push("success", "Saved");
    store.push("error", "Failed", "details");

    const toasts = useToastStore.getState().toasts;

    expect(toasts).toHaveLength(2);
    expect(toasts[0].kind).toBe("success");
    expect(toasts[0].title).toBe("Saved");
    expect(toasts[1].message).toBe("details");
    expect(toasts[1].id).toBeGreaterThan(toasts[0].id);
  });

  it("dismisses by id", () => {
    const store = useToastStore.getState();

    store.push("info", "One");
    store.push("info", "Two");

    const first = useToastStore.getState().toasts[0];

    useToastStore.getState().dismiss(first.id);

    const remaining = useToastStore.getState().toasts;

    expect(remaining).toHaveLength(1);
    expect(remaining[0].title).toBe("Two");
  });

  it("bounds the stack to the last five", () => {
    const store = useToastStore.getState();

    for (let i = 0; i < 8; i += 1) {
      store.push("info", `Toast ${i}`);
    }

    const toasts = useToastStore.getState().toasts;

    expect(toasts).toHaveLength(5);
    expect(toasts[0].title).toBe("Toast 3");
    expect(toasts[4].title).toBe("Toast 7");
  });
});

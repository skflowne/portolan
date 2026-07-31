export class Box<T> extends Array<T> implements Iterable<T> {
  get value(): T {
    throw new Error("not implemented");
  }

  get size(): number {
    return 1;
  }
}

export const members = { method(x: number): number { return x; }, prop: 1 };

export interface Weird extends Readonly<Record<string, unknown>>, Iterable<unknown> {
  optional?(): void;
}

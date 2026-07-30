export class Box<T> {
  get value(): T {
    throw new Error("not implemented");
  }

  get size(): number {
    return 1;
  }
}

export const members = { method(x: number): number { return x; }, prop: 1 };

export interface Weird {
  optional?(): void;
}

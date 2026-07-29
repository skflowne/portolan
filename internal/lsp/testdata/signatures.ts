export interface Callable {
  (value: string): number;
  new (value: string): Date;
  [key: string]: unknown;
}

export const assignedArrow = (x: number) => x + 1;

[1].map(x => x + 1);
[1].map(function (x): number { return x + 1; });

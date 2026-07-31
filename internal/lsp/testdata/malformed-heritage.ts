class A {}
class Base {}
class Broken extends Base<A,, B> {}
class MissingGenericOperand extends A<, A> {}
class MissingConstraintOperand<T extends, A> {}
class DuplicateCallOperand extends choose(A,, A) {}
class DuplicateObjectSeparator<T extends { first: A,, second: A }> {}

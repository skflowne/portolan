class A {}
class Base {}
class DuplicateGenericOperand extends Base<A,, B> {}
class Broken<T = A extends B ? C> {}
class Dup extends Base extends A {}
class Regex extends /x{1}/.constructor {}
class MissingGenericOperand extends A<, A> {}
class MissingConstraintOperand<T extends, A> {}
class DuplicateCallOperand extends choose(A,, A) {}
class DuplicateObjectSeparator<T extends { first: A,, second: A }> {}
class RepeatedUnion<T extends A | | B> {}
class RepeatedIntersection<T extends A & & B> {}
class MixedUnionIntersection<T extends A | & B> {}
class MixedIntersectionUnion<T extends A & | B> {}
interface MissingInterfaceSeparator extends A B {}
class MissingClassSeparator implements A B {}
class GenericAdjacency<T U> {}
class ParenthesizedAdjacency extends factory(A B) {}
class NestedGenericAdjacency extends A<B C> {}

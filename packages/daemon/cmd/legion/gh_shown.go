package main

// ghShown is place, the words that name value, a value a person gave legion gh (an argument, an
// option's value, a selector, an environment value, a search query), by its kind and where it
// stands ("a -R value", "a git flag after --", "an option the command's help does not list"). A
// refusal of legion gh prints no such value in any form: any of it can be a credential a person
// put there. value is passed so that each refusal path says which value its words stand for.
//
// TestNoRefusalPathPrintsACanary finds every call of ghShown in the source, keyed by the function
// that makes it and the source of its place argument (a literal, or a variable such as
// repositoryTarget's kind), and drives each with a canary in every slot.
// TestNoRefusalFormatsAPersonsValueItself holds every other place that formats a value to a
// reviewed table, each with the reason its value is none a person typed unchecked.
func ghShown(value, place string) string {
	return place
}

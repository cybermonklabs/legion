from fixture_py.greet import greet


def test_greet() -> None:
    assert greet("stage4b") == "hello, stage4b"


if __name__ == "__main__":
    test_greet()
    print(greet("stage4b"))

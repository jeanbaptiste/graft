# Federation

## What is Forge Federation?

Should a better distinction be made between **inter-forge collaboratioin** and **forge-federation**?

- **inter-forge collaboration** would refer to effective tools enabling a fraction a people willing to work together but from their own, separate forges. This would concern local or small teams to work on rather restricted projects with limited access and size.
- **forge-federation** would refer to a more global approach to **social-coding** (or social-hacking) enabling a wider audience to work and share content in the same way they discover new content, and connect with new peers through social platform such as Mastodon. This would concern more global scale open project.

## Mixed approaches

Let's review how to share code between platforms without a dedicated account, starting from the more common and easiest way.

1. Send a patch to a mailing list (Historical Unix/Linux approach, current SourceHut approach)
2. Use Git decentralize architecture (remotes)

## Using remotes

Collaborating between two Forgejo instances

- Two Forgejo instances f1 and f2
- **Alice** have a copy a ``federation-x`` repo on f1
- **Bob** have a copy a ``federation-x`` repo on f2

- Alice commit to federation-x and send a message to Bob asking to update his repo
- Bob add a ``git remote`` to Alice f1 and use ``git pull f1`` to get Alice contribution
- Alice can do the exact same thing on Bob f2
- Now, Alice and Bob can work on the same repo but on two different forge instances
- Question: does having a simple **sync/remote** button solve the collaboration between instances?
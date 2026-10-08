"""Application-level execution runtime shared by local and benchmark entry points.

This is the composition root for solver state. Domain models and persistence stay
in their own modules; callers own provisioning and teardown.
"""
from .bootstrap import SolverRuntime, bootstrap_solver

__all__ = ["SolverRuntime", "bootstrap_solver"]

# The tools

Generated from the registered tools (`make tools-doc`), with every tool registered; through muster every tool is `x_<server>_<name>`. `list_providers` is registered only with provider instances, `list_repositories` and `preview_workspace` only with provider instances and their Secrets, `authorize_workspace` only with a grant signing key.

| Tool | Kind |
|---|---|
| `authorize_workspace` | read-only |
| `create_workspace` | write |
| `delete_workspace` | write, destructive |
| `get_workspace` | read-only |
| `list_providers` | read-only |
| `list_repositories` | read-only |
| `list_workspaces` | read-only |
| `preview_workspace` | read-only |
| `sync_workspace` | write |
| `update_workspace` | write |

## `authorize_workspace`

```text
Check that you can read every repository of a workspace before you start a Session on it: a grant, or why not.

Granted: a short-lived grant to pass to start_session, naming the repositories pushed to since the last sync. Refused: the repositories you cannot read, or why the workspace cannot be started on yet. connect_required: the providers whose App to grant in muster first, and how. A granted check past the refresh threshold also requests a sync (syncRequested).
```

```json
{
  "properties": {
    "workspace": {
      "description": "The workspace's name",
      "type": "string"
    }
  },
  "required": [
    "workspace"
  ],
  "type": "object"
}
```

## `create_workspace`

```text
Create a workspace of an Organization you are a member of; it syncs after the save.

It syncs again when a Session starts on it and its last sync is older than the refresh threshold.
```

```json
{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The workspace's name: lower-case letters, digits and '-', unique in the installation"
    },
    "organization": {
      "type": "string",
      "description": "The Organization the workspace belongs to, for good; you must be a member"
    },
    "sources": {
      "type": [
        "null",
        "array"
      ],
      "items": {
        "type": "object",
        "properties": {
          "provider": {
            "type": "string"
          },
          "owner": {
            "type": "string"
          },
          "repositories": {
            "type": [
              "null",
              "array"
            ],
            "items": {
              "type": "string"
            }
          },
          "filters": {
            "type": [
              "null",
              "object"
            ],
            "properties": {
              "languages": {
                "type": [
                  "null",
                  "array"
                ],
                "items": {
                  "type": "string"
                }
              },
              "topics": {
                "type": [
                  "null",
                  "object"
                ],
                "properties": {
                  "match": {
                    "type": "string"
                  },
                  "names": {
                    "type": [
                      "null",
                      "array"
                    ],
                    "items": {
                      "type": "string"
                    }
                  }
                },
                "required": [
                  "names"
                ],
                "additionalProperties": false
              }
            },
            "additionalProperties": false
          },
          "exclude": {
            "type": [
              "null",
              "array"
            ],
            "items": {
              "type": "string"
            }
          },
          "includeArchived": {
            "type": "boolean"
          },
          "includeForks": {
            "type": "boolean"
          }
        },
        "required": [
          "provider",
          "owner"
        ],
        "additionalProperties": false
      },
      "description": "One owner on one provider instance each, with at least one selector: repositories by name, filters.languages or filters.topics"
    },
    "sizing": {
      "type": [
        "null",
        "object"
      ],
      "properties": {
        "headroom": {
          "type": "string",
          "description": "Replaces the computed headroom when larger, a Kubernetes quantity such as 8Gi"
        },
        "minimum": {
          "type": "string",
          "description": "The smallest size the volume gets, a Kubernetes quantity such as 32Gi"
        }
      },
      "description": "Overrides of the volume's computed size where its StorageClass provisions one",
      "additionalProperties": false
    }
  },
  "required": [
    "name",
    "organization",
    "sources"
  ],
  "additionalProperties": false
}
```

## `delete_workspace`

```text
Delete a workspace of your Organization: its sync stops and no new Session starts on it.

Returns the Sessions that still have a directory on its volume. On a workspace already being deleted, it runs the deletion cycle now: session directories found gone let the workspace and its volume go without waiting for the next cleanup date.
```

```json
{
  "properties": {
    "workspace": {
      "description": "The workspace's name",
      "type": "string"
    }
  },
  "required": [
    "workspace"
  ],
  "type": "object"
}
```

## `get_workspace`

```text
Get a workspace: its sources, last and running sync, mirrored repositories, volume storage and Session directories.

Mirror sizes are measured at the last sync; each Session directory comes with its size and cleanup date.
```

```json
{
  "properties": {
    "workspace": {
      "description": "The workspace's name",
      "type": "string"
    }
  },
  "required": [
    "workspace"
  ],
  "type": "object"
}
```

## `list_providers`

```text
List the installation's workspace providers, each with its kind and whether you granted its App in muster.

muster is the one place your provider sign-in lives.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `list_repositories`

```text
List one owner's repositories on a provider instance for a workspace's picker: searched by name, filtered and paged.

Filtered by language, topics, archived and forks; each with language, topics, last push and size, from the provider's cached listing; it says nothing about your own access to them.
```

```json
{
  "type": "object",
  "properties": {
    "organization": {
      "type": "string",
      "description": "The Organization whose workspace you edit; you must be a member"
    },
    "provider": {
      "type": "string",
      "description": "The provider instance's name, as list_providers shows it"
    },
    "owner": {
      "type": "string",
      "description": "The owner on the provider: an organization or a user account"
    },
    "search": {
      "type": "string",
      "description": "Only repositories whose name contains this, ignoring case"
    },
    "languages": {
      "type": [
        "null",
        "array"
      ],
      "items": {
        "type": "string"
      },
      "description": "Only repositories whose primary language is any of these"
    },
    "topics": {
      "type": [
        "null",
        "array"
      ],
      "items": {
        "type": "string"
      },
      "description": "Only repositories carrying any of these topics"
    },
    "archived": {
      "type": [
        "null",
        "boolean"
      ],
      "description": "true: only archived repositories; false: none; unset: both"
    },
    "fork": {
      "type": [
        "null",
        "boolean"
      ],
      "description": "true: only forks; false: none; unset: both"
    },
    "pageSize": {
      "type": "integer",
      "description": "Repositories per page: 50 by default, at most 200"
    }
  },
  "required": [
    "organization",
    "provider",
    "owner"
  ],
  "additionalProperties": false
}
```

## `list_workspaces`

```text
List the workspaces of your Organizations: sources, last sync, repository count, volume and Session directories.

volume carries its CSI handle; storedBytes is what the volume stored at the last sync, grewFrom the size a provisioned share grew from then. organizations names every Organization you are a member of, with or without a workspace: where you may create one.
```

```json
{
  "properties": {
    "organization": {
      "description": "Only this Organization's workspaces; you must be a member",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `preview_workspace`

```text
Preview the repositories a workspace selects now, an existing one or a draft, and its estimated volume size.

Paged, each repository with language, topics, last push and why it is in; also what the exclusions and the archived and fork rules left out. Writes nothing and says nothing about your own access to the repositories.
```

```json
{
  "type": "object",
  "properties": {
    "workspace": {
      "type": "string",
      "description": "An existing workspace to preview; or give organization and sources"
    },
    "organization": {
      "type": "string",
      "description": "The draft's Organization; you must be a member"
    },
    "sources": {
      "type": [
        "null",
        "array"
      ],
      "items": {
        "type": "object",
        "properties": {
          "provider": {
            "type": "string"
          },
          "owner": {
            "type": "string"
          },
          "repositories": {
            "type": [
              "null",
              "array"
            ],
            "items": {
              "type": "string"
            }
          },
          "filters": {
            "type": [
              "null",
              "object"
            ],
            "properties": {
              "languages": {
                "type": [
                  "null",
                  "array"
                ],
                "items": {
                  "type": "string"
                }
              },
              "topics": {
                "type": [
                  "null",
                  "object"
                ],
                "properties": {
                  "match": {
                    "type": "string"
                  },
                  "names": {
                    "type": [
                      "null",
                      "array"
                    ],
                    "items": {
                      "type": "string"
                    }
                  }
                },
                "required": [
                  "names"
                ],
                "additionalProperties": false
              }
            },
            "additionalProperties": false
          },
          "exclude": {
            "type": [
              "null",
              "array"
            ],
            "items": {
              "type": "string"
            }
          },
          "includeArchived": {
            "type": "boolean"
          },
          "includeForks": {
            "type": "boolean"
          }
        },
        "required": [
          "provider",
          "owner"
        ],
        "additionalProperties": false
      },
      "description": "The draft's sources, as create_workspace takes them"
    },
    "sizing": {
      "type": [
        "null",
        "object"
      ],
      "properties": {
        "headroom": {
          "type": "string",
          "description": "Replaces the computed headroom when larger, a Kubernetes quantity such as 8Gi"
        },
        "minimum": {
          "type": "string",
          "description": "The smallest size the volume gets, a Kubernetes quantity such as 32Gi"
        }
      },
      "description": "The draft's sizing overrides",
      "additionalProperties": false
    },
    "search": {
      "type": "string",
      "description": "Only selected repositories whose owner/name contains this, ignoring case"
    },
    "pageSize": {
      "type": "integer",
      "description": "Repositories per page: 50 by default, at most 200"
    }
  },
  "additionalProperties": false
}
```

## `sync_workspace`

```text
Request a sync of a workspace now: the controller fetches what changed in its sources and grows its volume.

The volume grows where its StorageClass provisions a size. Nothing changed costs no fetch; a request made while a sync runs is served after it. On a workspace being deleted it runs the deletion cycle now, which only cleans up the session directories. get_workspace shows the request, the running sync Job and the outcome.
```

```json
{
  "properties": {
    "workspace": {
      "description": "The workspace's name",
      "type": "string"
    }
  },
  "required": [
    "workspace"
  ],
  "type": "object"
}
```

## `update_workspace`

```text
Change a workspace's sources, and its sizing when given; its Organization never changes. It syncs after the save.
```

```json
{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The workspace's name"
    },
    "sources": {
      "type": [
        "null",
        "array"
      ],
      "items": {
        "type": "object",
        "properties": {
          "provider": {
            "type": "string"
          },
          "owner": {
            "type": "string"
          },
          "repositories": {
            "type": [
              "null",
              "array"
            ],
            "items": {
              "type": "string"
            }
          },
          "filters": {
            "type": [
              "null",
              "object"
            ],
            "properties": {
              "languages": {
                "type": [
                  "null",
                  "array"
                ],
                "items": {
                  "type": "string"
                }
              },
              "topics": {
                "type": [
                  "null",
                  "object"
                ],
                "properties": {
                  "match": {
                    "type": "string"
                  },
                  "names": {
                    "type": [
                      "null",
                      "array"
                    ],
                    "items": {
                      "type": "string"
                    }
                  }
                },
                "required": [
                  "names"
                ],
                "additionalProperties": false
              }
            },
            "additionalProperties": false
          },
          "exclude": {
            "type": [
              "null",
              "array"
            ],
            "items": {
              "type": "string"
            }
          },
          "includeArchived": {
            "type": "boolean"
          },
          "includeForks": {
            "type": "boolean"
          }
        },
        "required": [
          "provider",
          "owner"
        ],
        "additionalProperties": false
      },
      "description": "The workspace's sources, replacing the current ones"
    },
    "sizing": {
      "type": [
        "null",
        "object"
      ],
      "properties": {
        "headroom": {
          "type": "string",
          "description": "Replaces the computed headroom when larger, a Kubernetes quantity such as 8Gi"
        },
        "minimum": {
          "type": "string",
          "description": "The smallest size the volume gets, a Kubernetes quantity such as 32Gi"
        }
      },
      "description": "Replaces the sizing overrides when given ({} clears them); kept otherwise",
      "additionalProperties": false
    },
    "resourceVersion": {
      "type": "string",
      "description": "The resourceVersion get_workspace returned; the update is refused when the workspace changed since"
    }
  },
  "required": [
    "name",
    "sources"
  ],
  "additionalProperties": false
}
```

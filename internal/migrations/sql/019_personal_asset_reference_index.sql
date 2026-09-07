CREATE INDEX personal_node_asset_reference_idx ON asset_collection_items(asset_id)
  WHERE node_kind='file';

ALTER TABLE episodes
    DROP CONSTRAINT chk_episodes_cloud_publish_source,
    ADD CONSTRAINT chk_episodes_cloud_publish_source CHECK (
        cloud_publish_source IS NULL
        OR cloud_publish_source IN ('original', 'stereo_split', 'depth_normalization')
    );
